// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package search

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/typedapi/core/search"
	"github.com/elastic/go-elasticsearch/v8/typedapi/types"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/pkg/errors"
)

const (
	DefaultSearchTimeout = 30 * time.Second
	MaxSearchSize       = 10000
	DefaultMinScore     = 0.1
	DefaultMaxResults   = 100
	// Error handling constants
	MaxRetries        = 3
	RetryWaitTime     = 1 * time.Second
	CircuitBreakThreshold = 5
	CircuitBreakTimeout  = 30 * time.Second
)

// SearchError represents a custom error type for search operations
type SearchError struct {
	Op      string // Operation that failed
	Err     error  // Original error
	Context string // Additional context about the error
}

func (e *SearchError) Error() string {
	return fmt.Sprintf("%s: %v (context: %s)", e.Op, e.Err, e.Context)
}

// CircuitBreaker implements a simple circuit breaker pattern
type CircuitBreaker struct {
	failures     int
	lastFailure  time.Time
	threshold    int
	timeout      time.Duration
}

func NewCircuitBreaker(threshold int, timeout time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		threshold: threshold,
		timeout:   timeout,
	}
}

func (cb *CircuitBreaker) Allow() bool {
	if cb.failures >= cb.threshold {
		if time.Since(cb.lastFailure) < cb.timeout {
			return false
		}
		cb.Reset()
	}
	return true
}

func (cb *CircuitBreaker) RecordFailure() {
	cb.failures++
	cb.lastFailure = time.Now()
}

func (cb *CircuitBreaker) Reset() {
	cb.failures = 0
	cb.lastFailure = time.Time{}
}

type SearchEngine struct {
	client        *elastic.TypedClient
	cfg           *model.Config
	circuitBreaker *CircuitBreaker
}

func NewSearchEngine(client *elastic.TypedClient, cfg *model.Config) *SearchEngine {
	return &SearchEngine{
		client: client,
		cfg:    cfg,
		circuitBreaker: NewCircuitBreaker(CircuitBreakThreshold, CircuitBreakTimeout),
	}
}

type SearchOptions struct {
	Terms        string
	TeamId       string
	UserId       string
	IsOrSearch   bool
	FuzzyMatch   bool
	FuzzyPrefix  int
	FuzzyMaxExp  int
	MinScore     float64
	Highlight    bool
	HighlightPre string
	HighlightPost string
	// New fields for phrase matching and field boosting
	PhraseMatch   bool
	PhraseSlop    int
	FieldBoosts   map[string]float64
	Analyzer      string
	// Performance optimization fields
	Timeout       time.Duration
	MaxResults    int
	TrackTotalHits bool
	SearchAfter   []interface{}
	// Additional performance optimizations
	EnableCache    bool
	CacheTTL       time.Duration
	FilterFields   []string
	SortFields     []string
	Explain        bool
	Preference     string
	Routing        string
	SearchType     string
}

func (se *SearchEngine) SearchPosts(opts SearchOptions) (*model.PostSearchResults, error) {
	// Check circuit breaker
	if !se.circuitBreaker.Allow() {
		return nil, &SearchError{
			Op:      "SearchPosts",
			Err:     errors.New("circuit breaker open"),
			Context: "too many recent failures",
		}
	}

	// Validate input parameters
	if err := se.validateSearchOptions(opts); err != nil {
		return nil, err
	}

	// Set default values for performance options
	opts = se.setDefaultOptions(opts)

	query := buildSearchQuery(opts)
	searchRequest := se.buildSearchRequest(opts, query)

	// Create context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), opts.Timeout)
	defer cancel()

	// Add cache control if enabled
	if opts.EnableCache {
		ctx = context.WithValue(ctx, "cache-control", fmt.Sprintf("max-age=%d", int(opts.CacheTTL.Seconds())))
	}

	// Execute search with retries
	var res *search.Response
	var err error
	for i := 0; i < MaxRetries; i++ {
		res, err = se.client.Search().Index("posts").Request(searchRequest).Do(ctx)
		if err == nil {
			break
		}
		
		// Log retry attempt
		mlog.Warn("Search retry attempt",
			mlog.Int("attempt", i+1),
			mlog.Err(err),
		)
		
		if i < MaxRetries-1 {
			time.Sleep(RetryWaitTime * time.Duration(i+1))
		}
	}

	if err != nil {
		se.circuitBreaker.RecordFailure()
		return nil, &SearchError{
			Op:      "SearchPosts",
			Err:     err,
			Context: fmt.Sprintf("after %d retries", MaxRetries),
		}
	}

	// Reset circuit breaker on success
	se.circuitBreaker.Reset()

	results, err := se.processSearchResponse(res)
	if err != nil {
		return nil, &SearchError{
			Op:      "SearchPosts",
			Err:     err,
			Context: "processing search response",
		}
	}

	return results, nil
}

func (se *SearchEngine) validateSearchOptions(opts SearchOptions) error {
	if opts.Terms == "" {
		return &SearchError{
			Op:      "validateSearchOptions",
			Err:     errors.New("empty search terms"),
			Context: "search terms are required",
		}
	}
	return nil
}

func (se *SearchEngine) setDefaultOptions(opts SearchOptions) SearchOptions {
	if opts.Timeout == 0 {
		opts.Timeout = DefaultSearchTimeout
	}
	if opts.MaxResults == 0 {
		opts.MaxResults = DefaultMaxResults
	}
	if opts.MaxResults > MaxSearchSize {
		opts.MaxResults = MaxSearchSize
	}
	if opts.MinScore == 0 {
		opts.MinScore = DefaultMinScore
	}
	return opts
}

func (se *SearchEngine) buildSearchRequest(opts SearchOptions, query types.Query) *search.Request {
	searchRequest := &search.Request{
		Query: query,
		Size:  &opts.MaxResults,
		MinScore: &opts.MinScore,
		TrackTotalHits: &opts.TrackTotalHits,
		Explain: &opts.Explain,
	}

	// Add performance optimization settings
	if len(opts.SearchAfter) > 0 {
		searchRequest.SearchAfter = opts.SearchAfter
	}

	// Add field filtering if specified
	if len(opts.FilterFields) > 0 {
		searchRequest.Source = &types.SourceConfig{
			Includes: opts.FilterFields,
		}
	}

	// Add sorting if specified
	if len(opts.SortFields) > 0 {
		var sort []types.SortCombiner
		for _, field := range opts.SortFields {
			sort = append(sort, types.SortOptions{
				Field: field,
			})
		}
		searchRequest.Sort = sort
	}

	// Add search optimization settings
	if opts.Preference != "" {
		searchRequest.Preference = &opts.Preference
	}
	if opts.Routing != "" {
		searchRequest.Routing = &opts.Routing
	}
	if opts.SearchType != "" {
		searchRequest.SearchType = &opts.SearchType
	}

	if opts.Highlight {
		searchRequest.Highlight = &types.Highlight{
			Fields: map[string]types.HighlightField{
				"message": {
					PreTags:  []string{opts.HighlightPre},
					PostTags: []string{opts.HighlightPost},
					Type:     "unified",
				},
				"hashtags": {
					PreTags:  []string{opts.HighlightPre},
					PostTags: []string{opts.HighlightPost},
					Type:     "unified",
				},
			},
		}
	}

	return searchRequest
}

func (se *SearchEngine) processSearchResponse(res *search.Response) (*model.PostSearchResults, error) {
	results := &model.PostSearchResults{
		Posts:   make([]*model.Post, 0, len(res.Hits.Hits)),
		Matches: make(map[string][]string),
	}

	for _, hit := range res.Hits.Hits {
		var post model.Post
		if err := json.Unmarshal(hit.Source, &post); err != nil {
			mlog.Error("Failed to unmarshal post",
				mlog.String("post_id", hit.Id),
				mlog.Err(err),
			)
			continue
		}
		results.Posts = append(results.Posts, &post)

		// Add highlights to matches if available
		if hit.Highlight != nil {
			if messageHighlights, ok := hit.Highlight["message"]; ok {
				results.Matches[post.Id] = append(results.Matches[post.Id], messageHighlights...)
			}
			if hashtagHighlights, ok := hit.Highlight["hashtags"]; ok {
				results.Matches[post.Id] = append(results.Matches[post.Id], hashtagHighlights...)
			}
		}
	}

	results.TotalCount = int(res.Hits.Total.Value)
	return results, nil
}

func buildSearchQuery(opts SearchOptions) types.Query {
	var should []types.Query
	var must []types.Query

	// Add team filter
	if opts.TeamId != "" {
		must = append(must, types.Query{
			Term: map[string]types.TermQuery{
				"team_id": {Value: opts.TeamId},
			},
		})
	}

	// Add user filter if specified
	if opts.UserId != "" {
		must = append(must, types.Query{
			Term: map[string]types.TermQuery{
				"user_id": {Value: opts.UserId},
			},
		})
	}

	// Build message search query with field boosting
	messageBoost := float64(1.0)
	if boost, ok := opts.FieldBoosts["message"]; ok {
		messageBoost = boost
	}

	if opts.PhraseMatch {
		// Use match_phrase for exact phrase matching
		messageQuery := types.MatchPhraseQuery{
			Query: opts.Terms,
			Slop:  &opts.PhraseSlop,
		}
		if opts.Analyzer != "" {
			messageQuery.Analyzer = &opts.Analyzer
		}
		should = append(should, types.Query{
			MatchPhrase: map[string]types.MatchPhraseQuery{
				"message": messageQuery,
			},
			Boost: &messageBoost,
		})
	} else {
		// Use regular match query with fuzzy options
		messageQuery := types.MatchQuery{
			Query: opts.Terms,
			Type:  "best_fields",
		}
		if opts.FuzzyMatch {
			messageQuery.Fuzziness = "AUTO"
			messageQuery.PrefixLength = &opts.FuzzyPrefix
			messageQuery.MaxExpansions = &opts.FuzzyMaxExp
		}
		if opts.Analyzer != "" {
			messageQuery.Analyzer = &opts.Analyzer
		}
		should = append(should, types.Query{
			Match: map[string]types.MatchQuery{
				"message": messageQuery,
			},
			Boost: &messageBoost,
		})
	}

	// Build hashtag search query with field boosting
	hashtagBoost := float64(1.0)
	if boost, ok := opts.FieldBoosts["hashtags"]; ok {
		hashtagBoost = boost
	}

	if opts.PhraseMatch {
		// Use match_phrase for exact phrase matching
		hashtagQuery := types.MatchPhraseQuery{
			Query: opts.Terms,
			Slop:  &opts.PhraseSlop,
		}
		if opts.Analyzer != "" {
			hashtagQuery.Analyzer = &opts.Analyzer
		}
		should = append(should, types.Query{
			MatchPhrase: map[string]types.MatchPhraseQuery{
				"hashtags": hashtagQuery,
			},
			Boost: &hashtagBoost,
		})
	} else {
		// Use regular match query with fuzzy options
		hashtagQuery := types.MatchQuery{
			Query: opts.Terms,
			Type:  "best_fields",
		}
		if opts.FuzzyMatch {
			hashtagQuery.Fuzziness = "AUTO"
			hashtagQuery.PrefixLength = &opts.FuzzyPrefix
			hashtagQuery.MaxExpansions = &opts.FuzzyMaxExp
		}
		if opts.Analyzer != "" {
			hashtagQuery.Analyzer = &opts.Analyzer
		}
		should = append(should, types.Query{
			Match: map[string]types.MatchQuery{
				"hashtags": hashtagQuery,
			},
			Boost: &hashtagBoost,
		})
	}

	return types.Query{
		Bool: &types.BoolQuery{
			Must:   must,
			Should: should,
			MinimumShouldMatch: func() *int {
				if opts.IsOrSearch {
					v := 1
					return &v
				}
				v := len(should)
				return &v
			}(),
		},
	}
} 