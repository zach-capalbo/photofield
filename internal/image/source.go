package image

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"

	goio "io"

	"photofield/internal/clip"
	"photofield/internal/metrics"
	"photofield/internal/queue"
	"photofield/io"
	"photofield/io/ffmpeg"
	"photofield/io/ristretto"
	"photofield/io/sqlite"
	"photofield/tag"

	"github.com/docker/go-units"
	"github.com/golang/geo/s2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/sams96/rgeo"
)

var ErrNotFound = errors.New("not found")
var ErrNotAnImage = errors.New("not a supported image extension, might be video")
var ErrUnavailable = errors.New("unavailable")

type ImageId uint32

func IdsToUint32(ids <-chan ImageId) <-chan uint32 {
	out := make(chan uint32)
	go func() {
		for id := range ids {
			out <- uint32(id)
		}
		close(out)
	}()
	return out
}

func MissingInfoToInterface(c <-chan MissingInfo) <-chan interface{} {
	out := make(chan interface{})
	go func() {
		for m := range c {
			out <- interface{}(m)
		}
		close(out)
	}()
	return out
}

type SourcedInfo struct {
	Id ImageId
	Info
}

type Missing struct {
	Metadata  bool
	Color     bool
	Embedding bool
}

type IdPath struct {
	Id   ImageId
	Path string
}

type MissingInfo struct {
	Id   ImageId
	Path string
	Missing
}

type SimilarityInfo struct {
	SourcedInfo
	Similarity float32
}

func SimilarityInfosToSourcedInfos(sinfos <-chan SimilarityInfo) <-chan SourcedInfo {
	out := make(chan SourcedInfo)
	go func() {
		for sinfo := range sinfos {
			out <- sinfo.SourcedInfo
		}
		close(out)
	}()
	return out
}

type CacheConfig struct {
	MaxSize string `json:"max_size"`
}

func (config *CacheConfig) MaxSizeBytes() int64 {
	value, err := units.FromHumanSize(config.MaxSize)
	if err != nil {
		panic(err)
	}
	return value
}

type Caches struct {
	Image CacheConfig
}

type Geo struct {
	ReverseGeocode bool `json:"reverse_geocode"`
}

type Config struct {
	DataDir   string
	AI        clip.AI
	Geo       Geo
	TagConfig tag.Config `json:"-"`

	ExifToolCount        int  `json:"exif_tool_count"`
	SkipLoadInfo         bool `json:"skip_load_info"`
	ConcurrentMetaLoads  int  `json:"concurrent_meta_loads"`
	ConcurrentColorLoads int  `json:"concurrent_color_loads"`
	ConcurrentAILoads    int  `json:"concurrent_ai_loads"`

	ListExtensions []string        `json:"extensions"`
	DateFormats    []string        `json:"date_formats"`
	Images         FileConfig      `json:"images"`
	Videos         FileConfig      `json:"videos"`
	SourceTypes    SourceTypeMap   `json:"source_types"`
	Sources        SourceConfigs   `json:"sources"`
	Thumbnail      ThumbnailConfig `json:"thumbnail"`

	Caches Caches `json:"caches"`
}

type FileConfig struct {
	Extensions []string `json:"extensions"`
}

type Source struct {
	Config

	Sources                                    io.Sources
	SourceLatencyHistogram                     *prometheus.HistogramVec
	SourceLatencyAbsDiffHistogram              *prometheus.HistogramVec
	SourcePerOriginalMegapixelLatencyHistogram *prometheus.HistogramVec
	SourcePerResizedMegapixelLatencyHistogram  *prometheus.HistogramVec

	decoder  *Decoder
	database *Database
	rg       *rgeo.Rgeo

	imageInfoCache InfoCache
	pathCache      PathCache

	metadataQueue queue.Queue
	contentsQueue queue.Queue

	thumbnailSources    []io.ReadDecoder
	thumbnailGenerators io.Sources
	thumbnailSink       *sqlite.Source

	Clip clip.Clip
}

func NewSource(config Config, migrations embed.FS, migrationsThumbs embed.FS) *Source {
	source := Source{}
	source.Config = config
	source.decoder = NewDecoder(config.ExifToolCount)
	source.database = NewDatabase(filepath.Join(config.DataDir, "photofield.cache.db"), migrations)
	source.imageInfoCache = newInfoCache()
	source.pathCache = newPathCache()

	if config.Geo.ReverseGeocode {
		log.Println("rgeo loading")
		r, err := rgeo.New(rgeo.Provinces10, rgeo.Cities10)
		if err != nil {
			log.Fatalf("failed to initialize rgeo: %s", err)
		}
		source.rg = r
	}

	source.SourceLatencyHistogram = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: metrics.Namespace,
		Name:      "source_latency",
		Buckets:   []float64{500, 1000, 2500, 5000, 10000, 25000, 50000, 100000, 150000, 200000, 250000, 500000, 1000000, 2000000, 5000000, 10000000},
	},
		[]string{"source"},
	)

	source.SourceLatencyAbsDiffHistogram = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: metrics.Namespace,
		Name:      "source_latency_abs_diff",
		Buckets:   []float64{50, 100, 250, 500, 1000, 2500, 5000, 10000, 25000, 50000, 100000, 200000, 500000, 1000000},
	},
		[]string{"source"},
	)

	source.SourcePerOriginalMegapixelLatencyHistogram = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: metrics.Namespace,
		Name:      "source_per_original_megapixel_latency",
		Buckets:   []float64{500, 1000, 2500, 5000, 10000, 25000, 50000, 100000, 150000, 200000, 250000, 500000, 1000000, 2000000, 5000000, 10000000},
	},
		[]string{"source"},
	)

	source.SourcePerResizedMegapixelLatencyHistogram = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: metrics.Namespace,
		Name:      "source_per_resized_megapixel_latency",
		Buckets:   []float64{500, 1000, 2500, 5000, 10000, 25000, 50000, 100000, 150000, 200000, 250000, 500000, 1000000, 2000000, 5000000, 10000000},
	},
		[]string{"source"},
	)

	env := SourceEnvironment{
		SourceTypes: config.SourceTypes,
		FFmpegPath:  ffmpeg.FindPath(),
		Migrations:  migrationsThumbs,
		ImageCache:  ristretto.New(),
		DataDir:     config.DataDir,
	}

	// Sources used for rendering
	srcs, err := config.Sources.NewSources(&env)
	if err != nil {
		log.Fatalf("failed to create sources: %s", err)
	}
	source.Sources = srcs

	// Further sources should not be cached
	env.ImageCache = nil

	tsrcs, err := config.Thumbnail.Sources.NewSources(&env)
	if err != nil {
		log.Fatalf("failed to create thumbnail sources: %s", err)
	}
	for _, s := range tsrcs {
		rd, ok := s.(io.ReadDecoder)
		if !ok {
			log.Fatalf("source %s does not implement io.ReadDecoder", s.Name())
		}
		source.thumbnailSources = append(source.thumbnailSources, rd)
	}

	gens, err := config.Thumbnail.Generators.NewSources(&env)
	if err != nil {
		log.Fatalf("failed to create thumbnail generators: %s", err)
	}
	source.thumbnailGenerators = gens

	sink, err := config.Thumbnail.Sink.NewSource(&env)
	if err != nil {
		log.Fatalf("failed to create thumbnail sink: %s", err)
	}
	sqliteSink, ok := sink.(*sqlite.Source)
	if !ok {
		log.Fatalf("thumbnail sink %s is not a sqlite source", sink.Name())
	}
	source.thumbnailSink = sqliteSink

	if config.SkipLoadInfo {
		log.Printf("skipping load info")
	} else {

		source.metadataQueue = queue.Queue{
			ID:          "index_metadata",
			Name:        "index metadata",
			Worker:      source.indexMetadata,
			WorkerCount: config.ConcurrentMetaLoads,
		}
		go source.metadataQueue.Run()

		source.Clip = config.AI
		// }

		source.contentsQueue = queue.Queue{
			ID:          "index_contents",
			Name:        "index contents",
			Worker:      source.indexContents,
			WorkerCount: 8,
		}
		go source.contentsQueue.Run()

	}

	return &source
}

func (source *Source) ReverseGeocode(l s2.LatLng) (string, error) {
	if source.rg == nil {
		return "", ErrUnavailable
	}
	location, err := source.rg.ReverseGeocode([]float64{l.Lng.Degrees(), l.Lat.Degrees()})
	if err != nil {
		return "", err
	}
	loc := ""
	if err == nil {
		loc = location.City
		if loc == "" {
			loc = location.Province
		}
		if loc == "" {
			loc = location.Country
		} else if location.Country != "" {
			loc = fmt.Sprintf("%s (%s)", loc, location.Country)
		}
	}
	return loc, nil
}

func (source *Source) Vacuum() error {
	return source.database.vacuum()
}

func (source *Source) Close() {
	source.decoder.Close()
}

func (source *Source) IsSupportedImage(path string) bool {
	supportedImage := false
	pathExt := strings.ToLower(filepath.Ext(path))
	for _, ext := range source.Images.Extensions {
		if pathExt == ext {
			supportedImage = true
			break
		}
	}
	return supportedImage
}

func (source *Source) IsSupportedVideo(path string) bool {
	pathExt := strings.ToLower(filepath.Ext(path))
	for _, ext := range source.Videos.Extensions {
		if pathExt == ext {
			return true
		}
	}
	return false
}

func (source *Source) ListImages(dirs []string, maxPhotos int) <-chan string {
	for i := range dirs {
		dirs[i] = filepath.FromSlash(dirs[i])
	}
	return source.database.ListPaths(dirs, maxPhotos)
}

func (source *Source) ListImageIds(dirs []string, maxPhotos int) <-chan ImageId {
	for i := range dirs {
		dirs[i] = filepath.FromSlash(dirs[i])
	}
	return source.database.ListIds(dirs, maxPhotos, false)
}

func (source *Source) ListMissingEmbeddingIds(dirs []string, maxPhotos int) <-chan ImageId {
	for i := range dirs {
		dirs[i] = filepath.FromSlash(dirs[i])
	}
	return source.database.ListIds(dirs, maxPhotos, true)
}

func (source *Source) ListMissingMetadata(dirs []string, maxPhotos int, force Missing) <-chan MissingInfo {
	for i := range dirs {
		dirs[i] = filepath.FromSlash(dirs[i])
	}
	opts := Missing{
		Metadata: true,
	}
	if force.Metadata {
		opts = Missing{}
	}
	out := make(chan MissingInfo)
	go func() {
		for m := range source.database.ListMissing(dirs, maxPhotos, opts) {
			m.Metadata = m.Metadata || force.Metadata
			out <- m
		}
		close(out)
	}()
	return out
}

func (source *Source) ListMissingContents(dirs []string, maxPhotos int, force Missing) <-chan MissingInfo {
	for i := range dirs {
		dirs[i] = filepath.FromSlash(dirs[i])
	}
	opts := Missing{
		Color:     true,
		Embedding: source.AI.Available(),
	}
	if force.Color || force.Embedding {
		opts = Missing{}
	}
	out := make(chan MissingInfo)
	go func() {
		for m := range source.database.ListMissing(dirs, maxPhotos, opts) {
			m.Color = m.Color || force.Color
			m.Embedding = m.Embedding || force.Embedding
			out <- m
		}
		close(out)
	}()
	return out
}

func (source *Source) ListInfos(dirs []string, options ListOptions) <-chan SourcedInfo {
	for i := range dirs {
		dirs[i] = filepath.FromSlash(dirs[i])
	}
	out := make(chan SourcedInfo, 1000)
	go func() {
		defer metrics.Elapsed("list infos")()

		infos := source.database.List(dirs, options)
		for info := range infos {
			// if info.NeedsMeta() || info.NeedsColor() {
			// 	info.Info = source.GetInfo(info.Id)
			// }
			out <- info.SourcedInfo
		}
		close(out)
	}()
	return out
}

func (source *Source) ListInfosWithExistence(dirs []string, options ListOptions) <-chan SourcedInfo {
	for i := range dirs {
		dirs[i] = filepath.FromSlash(dirs[i])
	}
	out := make(chan SourcedInfo, 1000)
	go func() {
		defer metrics.Elapsed("list infos")()

		infos := source.database.List(dirs, options)
		for info := range infos {
			if info.NeedsMeta() || info.NeedsColor() {
				info.Info = source.GetInfo(info.Id)
			}
			out <- info.SourcedInfo
		}
		close(out)
	}()
	return out
}

// Prefer using ImageId over this unless you absolutely need the path
func (source *Source) GetImagePath(id ImageId) (string, error) {
	path, ok := source.pathCache.Get(id)
	if ok {
		return path, nil
	}

	path, ok = source.database.GetPathFromId(id)
	if !ok {
		return "", ErrNotFound
	}
	source.pathCache.Set(id, path)
	return path, nil
}

func (source *Source) GetImageEmbedding(id ImageId) (clip.Embedding, error) {
	return source.database.GetImageEmbedding(id)
}

func (source *Source) IndexFiles(dir string, max int, counter chan<- int) {
	dir = filepath.FromSlash(dir)
	indexed := make(map[string]struct{})
	for path := range walkFiles(dir, source.ListExtensions, max) {
		source.database.Write(path, Info{}, AppendPath)
		indexed[path] = struct{}{}
		// Uncomment to test slow indexing
		// time.Sleep(10 * time.Millisecond)
		counter <- 1
	}
	for ip := range source.database.ListNonexistent(dir, indexed) {
		source.database.Delete(ip.Id)
		source.thumbnailSink.Delete(uint32(ip.Id))
	}
	source.database.SetIndexed(dir)
	source.database.WaitForCommit()
}

func (source *Source) IndexMetadata(dirs []string, maxPhotos int, force Missing) {
	source.metadataQueue.AppendItems(MissingInfoToInterface(source.ListMissingMetadata(dirs, maxPhotos, force)))
}

func (source *Source) IndexContents(dirs []string, maxPhotos int, force Missing) {
	source.contentsQueue.AppendItems(MissingInfoToInterface(source.ListMissingContents(dirs, maxPhotos, force)))
}

func (source *Source) GetDir(dir string) Info {
	dir = filepath.FromSlash(dir)
	result, _ := source.database.GetDir(dir)
	return result.Info
}

func (source *Source) GetDirsCount(dirs []string) int {
	for i := range dirs {
		dirs[i] = filepath.FromSlash(dirs[i])
	}
	count, _ := source.database.GetDirsCount(dirs)
	return count
}

func (source *Source) GetImageReader(id ImageId, sourceName string, fn func(r goio.ReadSeeker, err error)) {
	ctx := context.TODO()
	path, err := source.GetImagePath(id)
	if err != nil {
		fn(nil, err)
		return
	}
	found := false
	for _, s := range source.Sources {
		if s.Name() != sourceName {
			continue
		}
		r, ok := s.(io.Reader)
		if !ok {
			continue
		}
		r.Reader(ctx, io.ImageId(id), path, func(r goio.ReadSeeker, err error) {
			// println(id, sourceName, s.Name(), r, ok, err)
			if err != nil {
				return
			}
			found = true
			fn(r, nil)
		})
		if found {
			break
		}
	}
	if !found {
		fn(nil, fmt.Errorf("unable to find image %d using %s", id, sourceName))
	}
}

func (source *Source) AddTag(name string) {
	done, _ := source.database.AddTag(name)
	<-done
}

func (source *Source) GetTag(name string) (tag.Tag, bool) {
	return source.database.GetTagByName(name)
}

func (source *Source) ListImageTags(id ImageId) <-chan tag.Tag {
	out := make(chan tag.Tag, 100)
	go func() {
		defer close(out)
		for tag := range source.database.ListImageTags(id) {
			out <- tag
		}
	}()
	return out
}

func (source *Source) ListTags(q string, limit int) <-chan tag.Tag {
	return source.database.ListTags(q, limit)
}

func (source *Source) AddTagIds(id tag.Id, ch <-chan ImageId) (rev int, err error) {
	ids := NewIds()
	for id := range ch {
		ids.AddInt(int(id))
	}
	rev, err = source.database.AddTagIds(id, ids)
	return
}

func (source *Source) RemoveTagIds(id tag.Id, ch <-chan ImageId) (rev int, err error) {
	ids := NewIds()
	for id := range ch {
		ids.AddInt(int(id))
	}
	rev, err = source.database.RemoveTagIds(id, ids)
	return
}

func (source *Source) InvertTagIds(id tag.Id, ch <-chan ImageId) (rev int, err error) {
	ids := NewIds()
	for id := range ch {
		ids.AddInt(int(id))
	}
	rev, err = source.database.InvertTagIds(id, ids)
	return
}

func (source *Source) GetTagId(name string) (tag.Id, bool) {
	return source.database.GetTagId(name)
}

func (source *Source) GetOrCreateTagFromNameRev(nameRev string) (tag.Tag, error) {
	t, err := tag.FromNameRev(nameRev)
	if err != nil {
		return tag.Tag{}, err
	}
	id, ok := source.GetTagId(t.Name)
	if ok {
		t.Id = id
	} else {
		source.AddTag(t.Name)
		id, ok := source.GetTagId(t.Name)
		if !ok {
			return tag.Tag{}, ErrNotFound
		}
		t.Id = id
	}
	return t, nil
}

func (source *Source) GetTagImageIds(id tag.Id) Ids {
	return source.database.GetTagImageIds(id)
}
