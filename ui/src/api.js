import useSWRV from "swrv";
import { computed, watch, ref } from "vue";
import qs from "qs";
import { useRetry } from "./use";

const host = import.meta.env.VITE_API_HOST || "/api";

async function fetcher(endpoint) {
  const response = await fetch(host + endpoint);
  if (!response.ok) {
    console.error(response);
    throw new Error(response.statusText);
  }
  return await response.json();
}

export async function get(endpoint, def) {
  const response = await fetch(host + endpoint);
  if (!response.ok) {
    if (def !== undefined) {
      return def;
    }
    console.error(response);
    throw new Error(response.statusText);
  }
  return await response.json();
}

export async function post(endpoint, body, def) {
  const response = await fetch(host + endpoint, {
    method: "POST",
    body: JSON.stringify(body),
    headers: {
      "Content-Type": "application/json; charset=utf-8",
    }
  });
  if (!response.ok) {
    if (def !== undefined) {
      return def;
    }
    console.error(response);
    throw new Error(response.statusText);
  }
  return await response.json();
}

export async function getRegions(sceneId, x, y, w, h) {
  if (!sceneId) return null;
  const response = await get(`/scenes/${sceneId}/regions?x=${x}&y=${y}&w=${w}&h=${h}`);
  return response.items;
}

export async function getRegion(sceneId, id) {
  return get(`/scenes/${sceneId}/regions/${id}`);
}

export async function getCenterRegion(sceneId, x, y, w, h) {
  const regions = await getRegions(sceneId, x, y, w, h);
  if (!regions) return null;
  const cx = x + w*0.5;
  const cy = y + h*0.5;
  let minDistSq = Infinity;
  let minRegion = null;
  for (let i = 0; i < regions.length; i++) {
    const region = regions[i];
    const rcx = region.bounds.x + region.bounds.w*0.5;
    const rcy = region.bounds.y + region.bounds.h*0.5;
    const dx = rcx - cx;
    const dy = rcy - cy;
    const distSq = dx*dx + dy*dy;
    if (distSq < minDistSq) {
      minDistSq = distSq;
      minRegion = region;
    }
  }
  return minRegion;
}

export async function getCollections() {
  return get(`/collections`);
}

export async function getCollection(id) {
  return get(`/collections/` + id);
}

export async function createTask(type, id) {
  return await post(`/tasks`, {
    type,
    collection_id: id
  });
}

export function getTileUrl(sceneId, level, x, y, tileSize, backgroundColor, extraParams) {
  const params = {
    tile_size: tileSize,
    zoom: level,
    background_color: backgroundColor,
    x,
    y,
    ...extraParams,
  };
  let url = `${host}/scenes/${sceneId}/tiles?${qs.stringify(params, { arrayFormat: "comma" })}`;
  return url;
}

export function getFileUrl(id, filename) {
  if (!filename) {
    return `${host}/files/${id}`;
  }
  return `${host}/files/${id}/original/${filename}`;
}

export async function getFileBlob(id) {
  return getBlob(`/files/` + id);
}

export function getThumbnailUrl(id, size, filename) {
  return `${host}/files/${id}/variants/${size}/${filename}`;
}

export function useApi(getUrl, config) {
  const response = useSWRV(getUrl, fetcher, config);
  const items = computed(() => response.data.value?.items);
  const itemsMutate = async getItems => {
    if (!getItems) {
      await response.mutate();
      return;
    } 
    const items = await getItems();
    await response.mutate(() => ({
      items,
    }));
  };
  return {
    ...response,
    items,
    itemsMutate,
  }
}

export function useScene({
  collectionId,
  layout,
  sort,
  imageHeight,
  viewport,
  search,
}) {
  
  const sceneParams = computed(() =>
    viewport?.width?.value &&
    viewport?.height?.value &&
    {
      layout: layout.value,
      sort: sort.value,
      image_height: imageHeight?.value || undefined,
      collection_id: collectionId.value,
      viewport_width: viewport.width.value,
      viewport_height: viewport.height.value,
      search: search?.value || undefined,
    }
  );

  const {
    items: scenes,
    isValidating: scenesLoading,
    itemsMutate: scenesMutate,
  } = useApi(() => sceneParams.value && `/scenes?` + qs.stringify(sceneParams.value));

  const scene = computed(() => {
    const list = scenes?.value;
    if (!list || list.length == 0) return null;
    return list[0];
  });

  const recreateScenesInProgress = ref(0);
  const recreateScene = async () => {
    recreateScenesInProgress.value = recreateScenesInProgress.value + 1;
    const params = sceneParams.value;
    await scenesMutate(async () => ([await createScene(params)]));
    recreateScenesInProgress.value = recreateScenesInProgress.value - 1;
  }

  watch(scenes, async newScene => {
    // Create scene if a matching one hasn't been found
    if (newScene?.length === 0) {
      console.log("scene not found, creating...");
      await recreateScene();
    }
  })

  const { run, reset } = useRetry(scenesMutate);

  const filesPerSecond = ref(0);
  watch(scene, async (newValue, oldValue) => {
    if (newValue?.loading) {
      let prev = oldValue?.file_count || 0;
      if (prev > newValue.file_count) {
        prev = 0;
      }
      filesPerSecond.value = newValue.file_count - prev;
      run();
    } else {
      reset();
      filesPerSecond.value = 0;
    }
  })

  return {
    scene,
    recreate: recreateScene,
    loading: scenesLoading,
    filesPerSecond,
  }
}

async function bufferFetcher(endpoint) {
  const response = await fetch(host + endpoint);
  if (!response.ok) {
    console.error(response);
    throw new Error(response.statusText);
  }
  return await response.arrayBuffer();
}

export function useBufferApi(getUrl, config) {
  return useSWRV(getUrl, bufferFetcher, config);
}

export function useTasks() {
  const intervalMs = 250;
  const response = useApi(
    () => `/tasks`
  );
  const { items, mutate } = response;
  const timer = ref(null);
  const resolves = ref([]);
  const updateUntilDone = async () => {
    await mutate();
    if (resolves.value) {
      return new Promise(resolve => resolves.value.push(resolve));
    }
    return;
  }
  watch(items, items => {
    if (items.length > 0) {
      if (!timer.value) {
        timer.value = setTimeout(() => {
          timer.value = null;
          mutate();
        }, intervalMs);
      }
    } else {
      resolves.value.forEach(resolve => resolve());
      resolves.value.length = 0;
    }
  })
  return {
    ...response,
    updateUntilDone,
  };
}

export async function createScene(params) {
  return await post(`/scenes`, params);
}

export async function addTag(body) {
  return await post(`/tags`, body);
}

export async function postTagFiles(id, body) {
  return await post(`/tags/${id}/files`, body);
}
