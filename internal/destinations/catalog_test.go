package destinations

import "testing"

func TestCatalogSelectionAndIsolation(t *testing.T) {
	lake, err := Resolve("")
	if err != nil || lake.ID != DefaultLakeID || lake.ProviderID != ProviderYodel {
		t.Fatalf("legacy selection = %+v, %v", lake, err)
	}
	for _, unknown := range []string{"unknown", " ", "BUNTZEN"} {
		if _, err := Resolve(unknown); err == nil {
			t.Fatalf("unsupported selection %q was accepted", unknown)
		}
	}
	lake.SupportedPasses[0] = "changed"
	listed := List()
	listed[0].SupportedPasses[0] = "also-changed"
	original, _ := Resolve(DefaultLakeID)
	if original.SupportedPasses[0] != "all_day" {
		t.Fatal("caller changed the destination registry")
	}
	rebased := original.WithOrigin("https://example.test/")
	if rebased.LoginURL != "https://example.test/buntzen-lake" || rebased.AllDayPassURL != "https://example.test/buntzen-lake/All-Day-Pass" {
		t.Fatalf("approved origin override = %+v", rebased)
	}
}
