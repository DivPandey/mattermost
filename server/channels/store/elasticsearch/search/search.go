// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package search

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/typedapi/core/search"
	"github.com/elastic/go-elasticsearch/v8/typedapi/types"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
)

type SearchEngine struct {
	client *elastic.TypedClient
	cfg    *model.Config
}

func NewSearchEngine(client *elastic.TypedClient, cfg *model.Config) *SearchEngine {
	return &SearchEngine{
		client: client,
		cfg:    cfg,
	}
}

func (se *SearchEngine) SearchPosts(terms string, teamId string, userId string, isOrSearch bool) (*model.PostSearchResults, error) {
	query := buildSearchQuery(terms, teamId, userId, isOrSearch)

	searchRequest := &search.Request{
		Query: query,
		Size:  &model.DefaultSearchPageSize,
	}

	res, err := se.client.Search().Index("posts").Request(searchRequest).Do(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to execute search: %w", err)
	}

	results := &model.PostSearchResults{
		Posts: make([]*model.Post, 0, len(res.Hits.Hits)),
	}

	for _, hit := range res.Hits.Hits {
		var post model.Post
		if err := json.Unmarshal(hit.Source, &post); err != nil {
			mlog.Error("Failed to unmarshal post", mlog.Err(err))
			continue
		}
		results.Posts = append(results.Posts, &post)
	}

	results.TotalCount = int(res.Hits.Total.Value)
	return results, nil
}

func buildSearchQuery(terms string, teamId string, userId string, isOrSearch bool) types.Query {
	var should []types.Query
	var must []types.Query

	// Add team filter
	if teamId != "" {
		must = append(must, types.Query{
			Term: map[string]types.TermQuery{
				"team_id": {Value: teamId},
			},
		})
	}

	// Add user filter if specified
	if userId != "" {
		must = append(must, types.Query{
			Term: map[string]types.TermQuery{
				"user_id": {Value: userId},
			},
		})
	}

	// Add message search
	should = append(should, types.Query{
		Match: map[string]types.MatchQuery{
			"message": {
				Query: terms,
				Type:  "best_fields",
			},
		},
	})

	// Add hashtag search
	should = append(should, types.Query{
		Match: map[string]types.MatchQuery{
			"hashtags": {
				Query: terms,
				Type:  "best_fields",
			},
		},
	})

	return types.Query{
		Bool: &types.BoolQuery{
			Must:   must,
			Should: should,
			MinimumShouldMatch: func() *int {
				if isOrSearch {
					v := 1
					return &v
				}
				v := len(should)
				return &v
			}(),
		},
	}
} 