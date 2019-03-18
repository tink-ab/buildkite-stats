package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"time"

	"github.com/buildkite/go-buildkite/buildkite"
)

type Build struct {
	ID          string
	Pipeline    Pipeline
	Branch      string
	ScheduledAt time.Time
	FinishedAt  time.Time
	StartedAt   time.Time
	CreatedAt   time.Time
}

type Pipeline struct {
	Name string
}

// Mapping to an internal struct will use a lot less memory.
func newBuildFromBuildkite(b buildkite.Build) Build {
	res := Build{
		ID: *b.ID,
		Pipeline: Pipeline{
			Name: *b.Pipeline.Name,
		},
		Branch: *b.Branch,

		// We can safely assumed that all timestamps are set in the input, as
		// we have a requirement that all builds should be finished when
		// querying from Buildkite.
		CreatedAt:   b.CreatedAt.Time,
		StartedAt:   b.StartedAt.Time,
		ScheduledAt: b.ScheduledAt.Time,
		FinishedAt:  b.FinishedAt.Time,
	}
	return res
}

type Buildkite interface {
	ListBuilds(from time.Time, p BuildPredicate) ([]Build, error)
}

type BuildPredicate interface {
	Predicate(Build) bool
}

type NetworkBuildkite struct {
	Client *buildkite.Client
	Org    string
	Cache  Cache
}

type Cache interface {
	Put(k string, v []byte, ttl time.Duration) error
	Get(k string) ([]byte, error)
}

const itemsPerPage = 100

func (b *NetworkBuildkite) ListBuilds(from time.Time, pred BuildPredicate) ([]Build, error) {
	to := time.Now()

	var res []Build
	for _, interval := range generateDailyIntervals(from, to) {
		log.Printf("Querying %+v...\n", interval)
		bs, err := b.listBuildsBetween(interval, cacheTTL(interval))
		if err != nil {
			return res, err
		}
		for _, b := range bs {
			if b.CreatedAt.After(from) && b.CreatedAt.Before(to) && pred.Predicate(b) {
				// Note that the daily intervals will be a superset of [to,
				// from). This is to get the cached buckets static. This means
				// that we need to do some filtering here.
				res = append(res, b)
			}
		}
	}

	return res, nil
}

func cacheTTL(interval timeInterval) time.Duration {
	if time.Now().Sub(interval.To) > 12*time.Hour {
		// Cache aggresively for older builds. We don't expect them to be
		// modified. Use spread to not have to reload all builds at the
		// same time.
		spread := time.Duration(rand.Intn(7*24)) * time.Hour
		return 60*24*time.Hour + spread
	} else {
		return 10 * time.Minute
	}
}

type timeInterval struct {
	From time.Time
	To   time.Time
}

func generateDailyIntervals(from, to time.Time) []timeInterval {
	startDay := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.Local)
	endDay := startDay.Add(24 * time.Hour)

	var res []timeInterval
	for startDay.Before(to) {
		res = append(res, timeInterval{startDay, endDay})
		startDay, endDay = startDay.Add(24*time.Hour), endDay.Add(24*time.Hour)
	}
	return res
}

func (b *NetworkBuildkite) listBuildsBetween(interval timeInterval, cacheTTL time.Duration) ([]Build, error) {
	cacheKey := fmt.Sprintf("%d-%d", interval.From.Unix(), interval.To.Unix())
	cached, err := b.readFromCache(cacheKey)
	if err == nil {
		return cached, err
	}

	opts := &buildkite.BuildsListOptions{
		ListOptions: buildkite.ListOptions{
			Page:    1,
			PerPage: itemsPerPage,
		},
		CreatedFrom: interval.From,
		CreatedTo:   interval.To,

		// This implies that all `Build`s will have FinishedAt set.
		State: []string{"passed"},
	}

	var result []Build
	for {
		builds, resp, err := b.query(b.Org, opts)
		if err != nil {
			return nil, err
		}

		result = append(result, builds...)

		if resp.NextPage <= 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}

	_ = b.populateCache(cacheKey, result, cacheTTL)

	return result, nil
}

func (b *NetworkBuildkite) query(org string, opts *buildkite.BuildsListOptions) ([]Build, *buildkite.Response, error) {
	bbuilds, resp, err := b.Client.Builds.ListByOrg(org, opts)
	if err != nil {
		return nil, resp, err
	}

	var result []Build
	for _, b := range bbuilds {
		result = append(result, newBuildFromBuildkite(b))
	}

	return result, resp, err
}

func (b *NetworkBuildkite) populateCache(key string, builds []Build, ttl time.Duration) error {
	s, err := json.Marshal(builds)
	if err != nil {
		log.Panicln(err)
	}

	// Compressing to make this a bit more future proof in case we have a _lot_
	// of builds per key one day - memcache keys usually can't be larger than 1
	// MB. We could of course switch to serialize to something like less
	// verbose like protobuf, but let's keep it simple for now.
	s = compress(s)

	return b.Cache.Put(key, s, ttl)
}

func (b *NetworkBuildkite) readFromCache(key string) ([]Build, error) {
	var res []Build
	s, err := b.Cache.Get(key)
	if err != nil {
		return res, err
	}

	s = decompress(s)

	err = json.Unmarshal(s, &res)
	if err != nil {
		log.Panicln(err)
	}

	return res, nil
}

func compress(b []byte) []byte {
	input := bytes.NewBuffer(b)
	output := bytes.NewBuffer(nil)
	r := gzip.NewWriter(output)
	_, _ = io.Copy(r, input)
	_ = r.Close()
	return output.Bytes()
}

func decompress(b []byte) []byte {
	input := bytes.NewBuffer(b)
	output := bytes.NewBuffer(nil)
	var err error
	r, err := gzip.NewReader(input)
	if err != nil {
		log.Panicln("unable to create gzip reader:", err)
	}
	_, err = io.Copy(output, r)
	if err != nil {
		log.Panicln("unable to decompress:", err)
	}
	err = r.Close()
	if err != nil {
		log.Panicln("unable to Close when decompressing:", err)
	}
	return output.Bytes()
}
