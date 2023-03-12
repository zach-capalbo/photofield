package image

import (
	"embed"
	"errors"
	"log"
	"path/filepath"
	"strings"
	"sync"

	"photofield/internal/clip"
	"photofield/internal/metrics"
	"photofield/internal/queue"
	"photofield/io"
	"photofield/io/cached"
	"photofield/io/ffmpeg"
	"photofield/io/goexif"
	ioimage "photofield/io/goimage"
	ioristretto "photofield/io/ristretto"
	"photofield/io/sqlite"
	"photofield/io/thumb"

	"github.com/dgraph-io/ristretto"
	"github.com/docker/go-units"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var ErrNotFound = errors.New("not found")
var ErrNotAnImage = errors.New("not a supported image extension, might be video")

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

type Config struct {
	DatabasePath       string
	DatabaseThumbsPath string
	AI                 clip.AI

	ExifToolCount        int  `json:"exif_tool_count"`
	SkipLoadInfo         bool `json:"skip_load_info"`
	ConcurrentMetaLoads  int  `json:"concurrent_meta_loads"`
	ConcurrentColorLoads int  `json:"concurrent_color_loads"`
	ConcurrentAILoads    int  `json:"concurrent_ai_loads"`

	ListExtensions []string    `json:"extensions"`
	DateFormats    []string    `json:"date_formats"`
	Images         FileConfig  `json:"images"`
	Videos         FileConfig  `json:"videos"`
	Thumbnails     []Thumbnail `json:"thumbnails"`

	Caches Caches `json:"caches"`
}

type FileConfig struct {
	Extensions []string `json:"extensions"`
}

type Source struct {
	Config

	Sources                 io.Sources
	SourcesLatencyHistogram *prometheus.HistogramVec
	Ristretto               ioristretto.Ristretto

	decoder  *Decoder
	database *Database

	imageInfoCache  InfoCache
	imageCache      ImageCache
	pathCache       PathCache
	fileExistsCache *ristretto.Cache

	imagesLoading      sync.Map
	imagesLoadingCount int

	MetaQueue queue.Queue
	// ColorQueue    queue.Queue
	ContentsQueue queue.Queue

	// ThumbnailSource  *sqlite.Source
	ThumbnailSources []io.ReadDecoder
	// ThumbnailGenerator  io.Source
	ThumbnailGenerators io.Sources
	ThumbnailSink       *sqlite.Source

	Clip clip.Clip
	// AIQueue queue.Queue
}

func NewSource(config Config, migrations embed.FS, migrationsThumbs embed.FS) *Source {
	source := Source{}
	source.Config = config
	source.decoder = NewDecoder(config.ExifToolCount)
	source.database = NewDatabase(config.DatabasePath, migrations)
	source.imageInfoCache = newInfoCache()
	source.imageCache = newImageCache(config.Caches)
	source.fileExistsCache = newFileExistsCache()
	source.pathCache = newPathCache()

	source.SourcesLatencyHistogram = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: metrics.Namespace,
		Name:      "source_latency",
		Buckets:   []float64{500, 1000, 2500, 5000, 10000, 25000, 50000, 100000, 250000, 500000, 1000000, 2000000, 5000000, 10000000},
	},
		[]string{"source"},
	)

	source.Ristretto = ioristretto.New()

	ffmpegPath := ffmpeg.FindPath()
	sqliteSource := sqlite.New(config.DatabaseThumbsPath, migrationsThumbs)

	source.ThumbnailSources = []io.ReadDecoder{
		sqliteSource,
		thumb.New(
			"SM",
			"{{.Dir}}@eaDir/{{.Filename}}/SYNOPHOTO_THUMB_SM.jpg",
			io.FitOutside,
			240,
			240,
		),
		goexif.Exif{},
		thumb.New(
			"S",
			"{{.Dir}}@eaDir/{{.Filename}}/SYNOPHOTO_THUMB_S.jpg",
			io.FitInside,
			120,
			120,
		),
	}

	source.ThumbnailSink = sqliteSource

	source.ThumbnailGenerators =
		io.Sources{
			ffmpeg.FFmpeg{
				Path:   ffmpegPath,
				Width:  256,
				Height: 256,
				Fit:    io.FitInside,
			},
			ioimage.Image{
				Width:  256,
				Height: 256,
			},
		}

	source.Sources = io.Sources{
		// ioristretto.New(),
		sqliteSource,
		goexif.Exif{},
		// cached.Cached{Source: goexif.Exif{}, Ristretto: source.Ristretto},
		thumb.New(
			"S",
			"{{.Dir}}@eaDir/{{.Filename}}/SYNOPHOTO_THUMB_S.jpg",
			io.FitInside,
			120,
			120,
		),
		thumb.New(
			"SM",
			"{{.Dir}}@eaDir/{{.Filename}}/SYNOPHOTO_THUMB_SM.jpg",
			io.FitOutside,
			240,
			240,
		),
		thumb.New(
			"M",
			"{{.Dir}}@eaDir/{{.Filename}}/SYNOPHOTO_THUMB_M.jpg",
			io.FitOutside,
			320,
			320,
		),
		thumb.New(
			"B",
			"{{.Dir}}@eaDir/{{.Filename}}/SYNOPHOTO_THUMB_B.jpg",
			io.FitInside,
			640,
			640,
		),
		thumb.New(
			"XL",
			"{{.Dir}}@eaDir/{{.Filename}}/SYNOPHOTO_THUMB_XL.jpg",
			io.FitOutside,
			1280,
			1280,
		),
		// exiftool.New("ThumbnailImage"),
		ioimage.Image{},
		ffmpeg.FFmpeg{
			Path:   ffmpegPath,
			Width:  256,
			Height: 256,
			Fit:    io.FitInside,
		},
		ffmpeg.FFmpeg{
			Path:   ffmpegPath,
			Width:  1280,
			Height: 1280,
			Fit:    io.FitInside,
		},
		ffmpeg.FFmpeg{
			Path:   ffmpegPath,
			Width:  4096,
			Height: 4096,
			Fit:    io.FitInside,
		},
	}

	for i := 0; i < len(source.Sources); i++ {
		source.Sources[i] = &cached.Cached{
			Source:    source.Sources[i],
			Ristretto: source.Ristretto,
		}
	}

	if config.SkipLoadInfo {
		log.Printf("skipping load info")
	} else {

		source.MetaQueue = queue.Queue{
			ID:          "index_metadata",
			Name:        "index metadata",
			Worker:      source.indexMetadata,
			WorkerCount: config.ConcurrentMetaLoads,
		}
		go source.MetaQueue.Run()

		source.Clip = config.AI
		// }

		source.ContentsQueue = queue.Queue{
			ID:          "index_contents",
			Name:        "index contents",
			Worker:      source.indexContents,
			WorkerCount: 8,
		}
		go source.ContentsQueue.Run()

	}

	return &source
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
	source.database.DeleteNonexistent(dir, indexed)
	source.database.SetIndexed(dir)
	source.database.WaitForCommit()
}

// func (source *Source) IndexAI(dirs []string, maxPhotos int) {
// 	source.AIQueue.AppendChan(IdsToUint32(source.ListMissingEmbeddingIds(dirs, maxPhotos)))
// }

func (source *Source) IndexMetadata(dirs []string, maxPhotos int, force Missing) {
	source.MetaQueue.AppendItems(MissingInfoToInterface(source.ListMissingMetadata(dirs, maxPhotos, force)))
}

func (source *Source) IndexContents(dirs []string, maxPhotos int, force Missing) {
	source.ContentsQueue.AppendItems(MissingInfoToInterface(source.ListMissingContents(dirs, maxPhotos, force)))
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

func (source *Source) GetApplicableThumbnails(path string) []Thumbnail {
	thumbs := make([]Thumbnail, 0, len(source.Thumbnails))
	pathExt := strings.ToLower(filepath.Ext(path))
	for _, t := range source.Thumbnails {
		supported := false
		for _, ext := range t.Extensions {
			if pathExt == ext {
				supported = true
				break
			}
		}
		if supported {
			thumbs = append(thumbs, t)
		}
	}
	return thumbs
}
