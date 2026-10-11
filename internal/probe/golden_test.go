package probe

import (
	"testing"
)

func TestMaskString(t *testing.T) {
	const base = "http://layout:5000"
	cases := []struct {
		name, in, want string
	}{
		{"plain text is kept", "prebuilt-layout", "prebuilt-layout"},
		{"a GUID", "0f8fad5b-d9cb-469f-a165-70867728950e", "<guid>"},
		{"an upper-case GUID inside a path",
			"/vision/v3.2/read/analyzeResults/0F8FAD5B-D9CB-469F-A165-70867728950E",
			"/vision/v3.2/read/analyzeResults/<guid>"},
		{"the container's own address and an operation id",
			"http://layout:5000/documentintelligence/documentModels/prebuilt-layout/analyzeResults/" +
				"00000001-0000-4000-8000-000000000001?api-version=2024-11-30",
			"<base>/documentintelligence/documentModels/prebuilt-layout/analyzeResults/<guid>?api-version=2024-11-30"},
		{"another host is kept", "http://other:5000/x", "http://other:5000/x"},
		{"a UTC timestamp keeps its layout", "2024-11-30T00:00:01Z", "<datetime:2006-01-02T15:04:05Z>"},
		{"fractional seconds keep their digits", "2024-11-30T09:15:42.1234567Z",
			"<datetime:2006-01-02T15:04:05.0000000Z>"},
		{"a numeric offset", "2024-11-30T09:15:42+05:30", "<datetime:2006-01-02T15:04:05-07:00>"},
		{"no zone at all", "2024-11-30T09:15:42", "<datetime:2006-01-02T15:04:05>"},
		{"a timestamp inside a sentence is kept", "created 2024-11-30T00:00:01Z", "created 2024-11-30T00:00:01Z"},
		{"a date alone is kept", "2024-11-30", "2024-11-30"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MaskString(tc.in, base); got != tc.want {
				t.Errorf("MaskString(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestMaskJSONKeepsOrderAndTypes(t *testing.T) {
	in := `{"status":"succeeded","createdDateTime":"2024-11-30T00:00:00Z","id":"0f8fad5b-d9cb-469f-a165-70867728950e",` +
		`"analyzeResult":{"z":1.50,"a":[true,null,"x"],"url":"http://h:1/a/0f8fad5b-d9cb-469f-a165-70867728950e"}}`
	want := `{"status":"succeeded","createdDateTime":"<datetime:2006-01-02T15:04:05Z>","id":"<guid>",` +
		`"analyzeResult":{"z":1.50,"a":[true,null,"x"],"url":"<base>/a/<guid>"}}`
	got, err := MaskJSON([]byte(in), "http://h:1")
	if err != nil {
		t.Fatalf("MaskJSON: %v", err)
	}
	if string(got) != want {
		t.Errorf("MaskJSON:\n got %s\nwant %s", got, want)
	}
}

func TestMaskJSONRejectsInvalidJSON(t *testing.T) {
	for _, in := range []string{`{"a":`, `{"a":1} {"b":2}`, ``} {
		if _, err := MaskJSON([]byte(in), ""); err == nil {
			t.Errorf("MaskJSON(%q) accepted invalid JSON", in)
		}
	}
}
