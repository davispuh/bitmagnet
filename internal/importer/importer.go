package importer

import (
	"context"
	"errors"
	"github.com/bitmagnet-io/bitmagnet/internal/database/dao"
	"github.com/bitmagnet-io/bitmagnet/internal/model"
	"github.com/bitmagnet-io/bitmagnet/internal/processor"
	"github.com/bitmagnet-io/bitmagnet/internal/protocol"
	"github.com/bitmagnet-io/bitmagnet/internal/queue/publisher"
	"gorm.io/gorm/clause"
	"sync"
	"time"
)

type Importer interface {
	New(ctx context.Context, info Info) ActiveImport
}

type Item struct {
	Source          string
	InfoHash        protocol.ID
	Name            string
	Size            uint64
	Private         bool
	ContentType     model.NullContentType
	ContentSource   model.NullString
	ContentID       model.NullString
	Title           model.NullString
	ReleaseDate     model.Date
	ReleaseYear     model.Year
	Episodes        model.Episodes
	VideoResolution model.NullVideoResolution
	VideoSource     model.NullVideoSource
	VideoCodec      model.NullVideoCodec
	Video3d         model.NullVideo3d
	VideoModifier   model.NullVideoModifier
	ReleaseGroup    model.NullString
	PublishedAt     time.Time
}

type Info struct {
	ID string
}

type importer struct {
	dao                *dao.Query
	processorPublisher publisher.Publisher[processor.MessageParams]
	bufferSize         uint
	maxWaitTime        time.Duration
}

var (
	ErrImportClosed = errors.New("import closed")
)

func (i importer) New(ctx context.Context, info Info) ActiveImport {
	ai := &activeImport{
		importer:        i,
		wg:              &sync.WaitGroup{},
		mutex:           &sync.RWMutex{},
		info:            info,
		itemChan:        make(chan Item),
		importedSources: make(map[string]struct{}),
	}
	ai.run(ctx)
	return ai
}

type ActiveImport interface {
	Import(items ...Item) error
	Drain()
	Closed() bool
	Close() error
	Err() error
	ImportedHashes() []protocol.ID
}

type ImportItemsError struct {
	Items []Item
	Err   error
}

type ImportErrors []ImportItemsError

func (e ImportErrors) Error() string {
	return "one or more items failed to import"
}

func (e ImportErrors) IsNil() bool {
	return len(e) == 0
}

func (e ImportErrors) OrNil() error {
	if e.IsNil() {
		return nil
	}
	return e
}

func (e ImportItemsError) Error() string {
	return e.Err.Error()
}

type activeImport struct {
	importer
	wg              *sync.WaitGroup
	stopped         bool
	mutex           *sync.RWMutex
	ctx             context.Context
	stop            context.CancelFunc
	info            Info
	itemChan        chan Item
	itemBuffer      []Item
	importedSources map[string]struct{}
	importedHashes  []protocol.ID
	errors          ImportErrors
}

func (i *activeImport) run(ctx context.Context) {
	i.mutex.Lock()
	go (func() {
		iCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		i.ctx = iCtx
		i.stop = cancel
		i.mutex.Unlock()
		for {
			select {
			case <-iCtx.Done():
				_ = i.Close()
				return
			case item, ok := <-i.itemChan:
				if !ok {
					return
				}
				go i.buffer(item)
			case <-time.After(i.maxWaitTime):
				go i.flush()
			}
		}
	})()
}

func (i *activeImport) buffer(item Item) {
	i.wg.Add(1)
	defer i.wg.Done()
	i.mutex.Lock()
	defer i.mutex.Unlock()
	i.itemBuffer = append(i.itemBuffer, item)
	if len(i.itemBuffer) >= int(i.bufferSize) {
		i.flushLocked()
	}
}

func (i *activeImport) flush() {
	i.mutex.Lock()
	defer i.mutex.Unlock()
	i.flushLocked()
}

func (i *activeImport) flushLocked() {
	if len(i.itemBuffer) == 0 {
		return
	}
	err := i.persistItems(i.itemBuffer...)
	if err != nil {
		i.errors = append(i.errors, ImportItemsError{
			Items: i.itemBuffer,
			Err:   err,
		})
	}
	i.itemBuffer = make([]Item, 0, i.bufferSize)
}

func (i *activeImport) persistItems(items ...Item) error {
	var sources []*model.TorrentSource
	sourcesMap := make(map[string]struct{})
	torrents := make([]*model.Torrent, 0, len(items))
	infoHashes := make([]protocol.ID, 0, len(items))
	for _, item := range items {
		if _, ok1 := i.importedSources[item.Source]; !ok1 {
			if _, ok2 := sourcesMap[item.Source]; !ok2 {
				sources = append(sources, &model.TorrentSource{
					Key:  item.Source,
					Name: item.Source,
				})
				sourcesMap[item.Source] = struct{}{}
			}
		}
		torrent := createTorrentModel(i.info, item)
		torrents = append(torrents, &torrent)
		infoHashes = append(infoHashes, item.InfoHash)
	}
	if len(sources) > 0 {
		if createSourcesErr := i.dao.TorrentSource.WithContext(i.ctx).Clauses(clause.OnConflict{
			DoNothing: true,
		}).CreateInBatches(sources, 100); createSourcesErr != nil {
			return createSourcesErr
		}
		for _, s := range sources {
			i.importedSources[s.Key] = struct{}{}
		}
	}
	if createTorrentsErr := i.dao.Torrent.WithContext(i.ctx).Clauses(clause.OnConflict{
		// todo work out how to handle conflicts here
		UpdateAll: true,
	}).CreateInBatches(torrents, 100); createTorrentsErr != nil {
		return createTorrentsErr
	}
	_, publishErr := i.processorPublisher.Publish(i.ctx, processor.MessageParams{
		InfoHashes: infoHashes,
	})
	if publishErr != nil {
		return publishErr
	}
	i.importedHashes = append(i.importedHashes, infoHashes...)
	return nil
}

func createTorrentModel(info Info, item Item) model.Torrent {
	t := model.Torrent{
		InfoHash:    item.InfoHash,
		Name:        item.Name,
		Size:        item.Size,
		Private:     item.Private,
		FilesStatus: model.FilesStatusNoInfo,
		Sources: []model.TorrentsTorrentSource{
			{
				Source:      item.Source,
				ImportID:    model.NewNullString(info.ID),
				PublishedAt: item.PublishedAt,
			},
		},
	}
	if item.ContentType.Valid {
		t.Hint = model.TorrentHint{
			ContentType:     item.ContentType.ContentType,
			ContentSource:   item.ContentSource,
			ContentID:       item.ContentID,
			Title:           item.Title,
			ReleaseYear:     item.ReleaseYear,
			Episodes:        item.Episodes,
			VideoResolution: item.VideoResolution,
			VideoSource:     item.VideoSource,
			VideoCodec:      item.VideoCodec,
			Video3d:         item.Video3d,
			VideoModifier:   item.VideoModifier,
			ReleaseGroup:    item.ReleaseGroup,
		}
	}
	return t
}

func (i *activeImport) Import(items ...Item) error {
	i.mutex.Lock()
	defer i.mutex.Unlock()
	if i.stopped {
		return ErrImportClosed
	}
	for _, item := range items {
		i.itemChan <- item
	}
	return nil
}

func (i *activeImport) Drain() {
	i.wg.Wait()
}

func (i *activeImport) Err() error {
	i.mutex.RLock()
	defer i.mutex.RUnlock()
	return i.errors.OrNil()
}

func (i *activeImport) ImportErrors() ImportErrors {
	i.mutex.RLock()
	defer i.mutex.RUnlock()
	return i.errors
}

func (i *activeImport) Closed() bool {
	i.mutex.RLock()
	defer i.mutex.RUnlock()
	return i.stopped
}

func (i *activeImport) Close() error {
	i.mutex.Lock()
	defer i.mutex.Unlock()
	i.flushLocked()
	if !i.stopped {
		i.stopped = true
		i.stop()
		close(i.itemChan)
	}
	return i.errors.OrNil()
}

func (i *activeImport) ImportedHashes() []protocol.ID {
	i.mutex.RLock()
	defer i.mutex.RUnlock()
	return i.importedHashes
}
