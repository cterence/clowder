package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestFetchDERPMapCustomURL(t *testing.T) {
	const regionJSON = `{
		"Regions": {
			"99": {
				"RegionID": 99,
				"RegionCode": "tst",
				"RegionName": "test relay",
				"Nodes": [
					{"Name": "t1", "RegionID": 99, "RegionCode": "tst", "HostName": "127.0.0.1:1"}
				]
			}
		}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(regionJSON))
	}))
	defer srv.Close()

	dm, err := fetchDERPMap(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("fetchDERPMap with a custom URL: %v", err)
	}
	if dm.Regions[99] == nil {
		t.Fatalf("custom DERP map not fetched: %+v", dm.Regions)
	}
	if dm.Regions[99].RegionCode != "tst" {
		t.Fatalf("region 99 code = %q, want tst", dm.Regions[99].RegionCode)
	}
}

func TestPairRegions(t *testing.T) {
	const regionJSON = `{
		"Regions": {
			"1":   {"RegionID": 1,   "RegionCode": "a", "RegionName": "a", "Nodes": [{"Name": "a", "RegionID": 1,   "RegionCode": "a", "HostName": "127.0.0.1:1"}]},
			"99":  {"RegionID": 99,  "RegionCode": "b", "RegionName": "b", "Nodes": [{"Name": "b", "RegionID": 99,  "RegionCode": "b", "HostName": "127.0.0.1:1"}]},
			"303": {"RegionID": 303, "RegionCode": "c", "RegionName": "c", "Nodes": [{"Name": "c", "RegionID": 303, "RegionCode": "c", "HostName": "127.0.0.1:1"}]}
		}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(regionJSON))
	}))
	defer srv.Close()

	got, err := pairRegions(context.Background(), srv.URL, 99)
	if err != nil {
		t.Fatalf("pairRegions: %v", err)
	}
	want := []int{99, 1, 99, 303, 99}
	if !slices.Equal(got, want) {
		t.Fatalf("pairRegions(encoded 99) = %v, want %v", got, want)
	}

	// An encoded region the map does not know still leads and gets
	// retried between every other region.
	got, err = pairRegions(context.Background(), srv.URL, 55)
	if err != nil {
		t.Fatalf("pairRegions: %v", err)
	}
	if !slices.Equal(got, []int{55, 1, 55, 99, 55, 303, 55}) {
		t.Fatalf("pairRegions(encoded 55) = %v, want [55 1 55 99 55 303 55]", got)
	}
}
