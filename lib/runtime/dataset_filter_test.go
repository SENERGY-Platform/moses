package runtime

import (
	"testing"

	"github.com/SENERGY-Platform/moses/lib/domain"
)

func TestTheFiltersOfADocumentReachTheQuery(t *testing.T) {
	//the gap this closes is between the document and the request: a filter the
	//api accepts but remoteQuery drops would silently replay every station
	runtime := &Runtime{
		fetcher:    &fakeFetcher{},
		ownerToken: func(string) (string, error) { return "token", nil },
	}
	source := &domain.DatasetSource{
		Origin:  domain.OriginExport,
		Ref:     "export-1",
		Filters: []domain.DatasetFilter{{Column: "station_id", Value: "02932"}},
	}
	_, series, err := runtime.remoteQuery("owner", source)
	if err != nil {
		t.Fatal(err)
	}
	if len(series.Filters) != 1 || series.Filters[0].Column != "station_id" || series.Filters[0].Value != "02932" {
		t.Fatalf("expected the document's filter on the series, got %+v", series.Filters)
	}

	//a device series has nothing to narrow and keeps none
	device := &domain.DatasetSource{Origin: domain.OriginPlatform, Ref: "device-1", ServiceRef: "service-1"}
	if _, series, err = runtime.remoteQuery("owner", device); err != nil {
		t.Fatal(err)
	}
	if len(series.Filters) != 0 {
		t.Errorf("a device series carries no filters, got %+v", series.Filters)
	}
}
