package photofield

import (
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/gosimple/slug"

	. "photofield/internal"
	. "photofield/internal/storage"
)

type Collection struct {
	Id            string   `json:"id"`
	Name          string   `json:"name"`
	ListLimit     int      `json:"list_limit"`
	ExpandSubdirs bool     `json:"expand_subdirs"`
	ExpandSort    string   `json:"expand_sort"`
	Dirs          []string `json:"dirs"`
}

func (collection *Collection) GenerateId() {
	collection.Id = slug.Make(collection.Name)
}

func (collection *Collection) Expand() []Collection {
	collections := make([]Collection, 0)
	for _, photoDir := range collection.Dirs {
		dir, err := os.Open(photoDir)
		if err != nil {
			log.Fatalln("Unable to expand dir", photoDir)
		}
		defer dir.Close()

		list, _ := dir.Readdirnames(0)
		for _, name := range list {
			child := Collection{
				Name:      name,
				Dirs:      []string{filepath.Join(photoDir, name)},
				ListLimit: collection.ListLimit,
			}
			collections = append(collections, child)
		}
	}
	switch collection.ExpandSort {
	case "asc":
		sort.Slice(collections, func(i, j int) bool {
			return collections[i].Name < collections[j].Name
		})
	case "desc":
		sort.Slice(collections, func(i, j int) bool {
			return collections[i].Name > collections[j].Name
		})
	}
	return collections
}

func (collection *Collection) GetIds(source *ImageSource) <-chan ImageId {
	out := make(chan ImageId)
	go func() {
		for path := range collection.GetPaths(source) {
			out <- source.GetImageId(path)
		}
		close(out)
	}()
	return out
}

func (collection *Collection) GetPaths(source *ImageSource) <-chan string {
	listingFinished := Elapsed("listing")
	out := make(chan string)
	wg := &sync.WaitGroup{}
	wg.Add(len(collection.Dirs))
	for _, photoDir := range collection.Dirs {
		go source.ListImages(photoDir, collection.ListLimit, out, wg)
	}
	go func() {
		wg.Wait()
		listingFinished()
		close(out)
	}()
	return out
}
