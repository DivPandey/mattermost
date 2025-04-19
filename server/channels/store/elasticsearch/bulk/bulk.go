// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package bulk

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/typedapi/core/bulk"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
)

const (
	BulkIndexingBatchSize = 1000
	BulkIndexingTimeout   = 5 * time.Second
)

type BulkProcessor struct {
	client      *elastic.TypedClient
	cfg         *model.Config
	mutex       sync.Mutex
	actions     []map[string]any
	lastFlush   time.Time
	stopChan    chan struct{}
	wg          sync.WaitGroup
}

func NewBulkProcessor(client *elastic.TypedClient, cfg *model.Config) *BulkProcessor {
	return &BulkProcessor{
		client:    client,
		cfg:       cfg,
		stopChan:  make(chan struct{}),
		lastFlush: time.Now(),
	}
}

func (bp *BulkProcessor) Start() {
	bp.wg.Add(1)
	go bp.run()
}

func (bp *BulkProcessor) Stop() {
	close(bp.stopChan)
	bp.wg.Wait()
}

func (bp *BulkProcessor) run() {
	defer bp.wg.Done()

	ticker := time.NewTicker(BulkIndexingTimeout)
	defer ticker.Stop()

	for {
		select {
		case <-bp.stopChan:
			bp.flush()
			return
		case <-ticker.C:
			bp.flush()
		}
	}
}

func (bp *BulkProcessor) Add(action map[string]any) {
	bp.mutex.Lock()
	defer bp.mutex.Unlock()

	bp.actions = append(bp.actions, action)

	if len(bp.actions) >= BulkIndexingBatchSize {
		bp.flush()
	}
}

func (bp *BulkProcessor) flush() {
	bp.mutex.Lock()
	if len(bp.actions) == 0 {
		bp.mutex.Unlock()
		return
	}

	actions := bp.actions
	bp.actions = make([]map[string]any, 0, BulkIndexingBatchSize)
	bp.lastFlush = time.Now()
	bp.mutex.Unlock()

	if len(actions) == 0 {
		return
	}

	bulkRequest := &bulk.Request{
		Operations: actions,
	}

	res, err := bp.client.Bulk().Request(bulkRequest).Do(context.Background())
	if err != nil {
		mlog.Error("Failed to execute bulk request", mlog.Err(err))
		return
	}

	if res.Errors {
		for _, item := range res.Items {
			for _, action := range item {
				if action.Error != nil {
					mlog.Error("Bulk indexing error",
						mlog.String("index", action.Index),
						mlog.String("id", action.Id),
						mlog.String("error", action.Error.Reason),
					)
				}
			}
		}
	}
} 