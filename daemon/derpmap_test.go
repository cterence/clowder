package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
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
