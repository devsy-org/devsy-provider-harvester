package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const readyImage = `{"metadata":{"name":"image-a","namespace":"default"},"spec":{"displayName":"Ubuntu Noble"},"status":{"storageClassName":"sc-a","conditions":[{"type":"Imported","status":"True"}]}}`

func imageFixture(t *testing.T, object, list string) string {
	t.Helper()
	exe := lifecycleFixture(t)
	dir := t.TempDir()
	for name, data := range map[string]string{"object": object, "list": list} {
		file := filepath.Join(dir, name+".json")
		if err := os.WriteFile(file, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if name == "object" {
			t.Setenv("HARVESTER_TEST_IMAGE_OBJECT", file)
		} else {
			t.Setenv("HARVESTER_TEST_IMAGE_LIST", file)
		}
	}
	t.Setenv("HARVESTER_IMAGE_NAMESPACE", "default")
	return exe
}

func TestImageCatalogIsReadableSortedAndReadOnly(t *testing.T) {
	other := strings.ReplaceAll(strings.ReplaceAll(readyImage, "image-a", "image-b"), "Ubuntu Noble", "Alpine")
	exe := imageFixture(t, "", `{"items":[`+readyImage+`,`+other+`]}`)
	calls := filepath.Join(t.TempDir(), "calls.jsonl")
	t.Setenv("HARVESTER_TEST_KUBECTL_CALLS", calls)
	out, stderr, err := runLifecycleFixture(t, exe, "images", "--json")
	if err != nil || stderr != "" {
		t.Fatalf("catalog failed: %q %v", stderr, err)
	}
	var entries []struct {
		DisplayName string `json:"displayName"`
		Reference   string `json:"reference"`
		Ready       bool   `json:"ready"`
	}
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].DisplayName != "Alpine" || entries[1].Reference != "default/image-a" || !entries[1].Ready {
		t.Fatalf("unexpected catalog: %s", out)
	}
	data, err := os.ReadFile(calls)
	if err != nil || !strings.Contains(string(data), `"virtualmachineimages","-n","default"`) || strings.Count(string(data), "\n") != 1 {
		t.Fatalf("catalog must only list images in the selected namespace: %s %v", data, err)
	}
}

func TestImageDisplayNameResolvesToResource(t *testing.T) {
	imageFixture(t, "", `{"items":[`+readyImage+`]}`)
	c := load()
	c.Image = "Ubuntu Noble"
	image, err := resolveImage(c)
	if err != nil || image.Name != "image-a" || image.Namespace != "default" || image.StorageClass != "sc-a" {
		t.Fatalf("display name did not resolve to pinned resource: %#v %v", image, err)
	}
}

func TestImageSelectionRejectsAmbiguousMissingAndUnready(t *testing.T) {
	duplicate := strings.ReplaceAll(readyImage, "image-a", "image-b")
	for _, tc := range []struct{ name, object, list, selector, want string }{
		{"duplicate", "", `{"items":[` + readyImage + `,` + duplicate + `]}`, "Ubuntu Noble", "ambiguous"},
		{"missing", "", `{"items":[]}`, "Missing image", "not found"},
		{"import incomplete", strings.Replace(readyImage, `"status":"True"`, `"status":"False"`, 1), `{"items":[]}`, "default/image-a", "not ready"},
		{"storage unavailable", strings.Replace(readyImage, `"storageClassName":"sc-a"`, `"storageClassName":""`, 1), `{"items":[]}`, "default/image-a", "not ready"},
		{"deleting", strings.Replace(readyImage, `"namespace":"default"`, `"namespace":"default","deletionTimestamp":"2026-10-20T00:00:00Z"`, 1), `{"items":[]}`, "default/image-a", "not ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			imageFixture(t, tc.object, tc.list)
			c := load()
			c.Image = tc.selector
			_, err := resolveImage(c)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %s, got %v", tc.want, err)
			}
		})
	}
}

func TestImageReferencesKeepPriorityAndDoNotRemapMissingPinnedID(t *testing.T) {
	for _, selector := range []string{"image-a", "default/image-a"} {
		t.Run(selector, func(t *testing.T) {
			imageFixture(t, readyImage, `{"items":[]}`)
			c := load()
			c.Image = selector
			image, err := resolveImage(c)
			if err != nil || image.Name != "image-a" {
				t.Fatalf("legacy reference rejected: %#v %v", image, err)
			}
		})
	}
	t.Run("deleted pinned ID", func(t *testing.T) {
		other := strings.Replace(strings.Replace(readyImage, "image-a", "image-b", 1), "Ubuntu Noble", "image-a", 1)
		imageFixture(t, "", `{"items":[`+other+`]}`)
		c := load()
		c.Image = "default/image-a"
		if _, err := resolveImage(c); err == nil {
			t.Fatal("deleted namespaced ID silently remapped to a display name")
		}
	})
}

func TestImageCatalogAPIFailureIsNotAnEmptyCatalog(t *testing.T) {
	exe := imageFixture(t, "", `{"items":[]}`)
	t.Setenv("HARVESTER_TEST_IMAGE_LIST_FAILURE", "1")
	_, stderr, err := runLifecycleFixture(t, exe, "images", "--json")
	if err == nil || !strings.Contains(stderr, "forbidden") {
		t.Fatalf("catalog API error hidden: %q %v", stderr, err)
	}
}

func TestOptionalImagePickerReportsFailureWithoutBlockingSavedImage(t *testing.T) {
	exe := imageFixture(t, readyImage, `{"items":[]}`)
	t.Setenv("HARVESTER_TEST_IMAGE_LIST_FAILURE", "1")
	t.Setenv("HARVESTER_IMAGE_PIN", "default/image-a")
	out, stderr, err := runLifecycleFixture(t, exe, "image-options")
	if err != nil || stderr != "" || !strings.Contains(out, "catalog unavailable") || !strings.Contains(out, `"default":"default/image-a"`) {
		t.Fatalf("optional catalog blocked the existing selection or hid its failure: %q %q %v", out, stderr, err)
	}
	image, err := resolveImage(load())
	if err != nil || image.Name != "image-a" {
		t.Fatalf("exact image reference must still work with get-only access: %#v %v", image, err)
	}
}

func TestImagePickerDoesNotClearSavedSelectionOnConnectionFailure(t *testing.T) {
	exe := imageFixture(t, readyImage, `{"items":[]}`)
	t.Setenv("HARVESTER_TEST_IMAGE_LIST_FAILURE", "1")
	t.Setenv("HARVESTER_TEST_IMAGE_FAILURE", "1")
	t.Setenv("HARVESTER_IMAGE_PIN", "default/image-a")
	out, stderr, err := runLifecycleFixture(t, exe, "image-options")
	if err == nil || out != "" || !strings.Contains(stderr, "cannot verify saved image") {
		t.Fatalf("connection failure must stop resolution before clearing selection: %q %q %v", out, stderr, err)
	}
}

func TestImageOptionsNeverSelectFirstImageAndPreserveAvailablePin(t *testing.T) {
	exe := imageFixture(t, "", `{"items":[`+readyImage+`]}`)
	for _, pin := range []string{"", "default/image-a", "other/image-old"} {
		t.Setenv("HARVESTER_IMAGE_PIN", pin)
		out, stderr, err := runLifecycleFixture(t, exe, "image-options")
		if err != nil || stderr != "" {
			t.Fatalf("options failed: %q %v", stderr, err)
		}
		var payload struct {
			Options map[string]struct {
				Default string `json:"default"`
				Enum    []struct {
					Value       string `json:"value"`
					DisplayName string `json:"displayName"`
				} `json:"enum"`
			} `json:"options"`
		}
		if err := json.Unmarshal([]byte(out), &payload); err != nil {
			t.Fatal(err)
		}
		choice := payload.Options["HARVESTER_IMAGE_CHOICE"]
		want := pin
		if pin == "other/image-old" {
			want = ""
		}
		if choice.Default != want || len(choice.Enum) != 2 || choice.Enum[0].Value != "" || choice.Enum[0].DisplayName != "Select an image" || choice.Enum[1].Value != "default/image-a" || choice.Enum[1].DisplayName != "Ubuntu Noble" {
			t.Fatalf("selector must offer only ready images with a neutral placeholder: %s", out)
		}
	}
}

func TestImageOptionsClearChoiceOutsideCatalogWithoutRemapping(t *testing.T) {
	exe := imageFixture(t, "", `{"items":[]}`)
	t.Setenv("HARVESTER_IMAGE_CHOICE", "old-namespace/image-a")
	out, stderr, err := runLifecycleFixture(t, exe, "image-options")
	if err != nil || stderr != "" || !strings.Contains(out, `"default":""`) || strings.Contains(out, `"value":"old-namespace/image-a"`) {
		t.Fatalf("selector offered an image outside its ready catalog: %q %q %v", out, stderr, err)
	}
}

func TestImageSelectorMigratesExistingImageWithoutManualControl(t *testing.T) {
	for _, legacy := range []string{"image-a", "default/image-a", "Ubuntu Noble"} {
		t.Run(legacy, func(t *testing.T) {
			exe := imageFixture(t, "", `{"items":[`+readyImage+`]}`)
			t.Setenv("HARVESTER_IMAGE", legacy)
			out, _, err := runLifecycleFixture(t, exe, "image-options")
			if err != nil || !strings.Contains(out, `"default":"default/image-a"`) || !strings.Contains(out, `"displayName":"Image"`) || strings.Contains(strings.ToLower(out), "manual") {
				t.Fatalf("existing selection was not migrated into one image control: %q %v", out, err)
			}
		})
	}
}

func TestClearingCatalogSelectionCannotReactivateLegacyImage(t *testing.T) {
	lifecycleFixture(t)
	t.Setenv("HARVESTER_IMAGE", "old-image")
	t.Setenv("HARVESTER_IMAGE_PIN", "")
	t.Setenv("HARVESTER_IMAGE_SELECTION_MODE", "catalog")
	if got := load().Image; got != "" {
		t.Fatalf("empty selector silently activated hidden legacy value %q", got)
	}
}

func TestImageOptionsExcludeUnreadyNewChoices(t *testing.T) {
	unready := strings.Replace(readyImage, `"status":"True"`, `"status":"False"`, 1)
	exe := imageFixture(t, "", `{"items":[`+unready+`]}`)
	out, _, err := runLifecycleFixture(t, exe, "image-options")
	if err != nil || strings.Contains(out, `"value":"default/image-a"`) {
		t.Fatalf("unfinished import offered as new image: %q %v", out, err)
	}
	out, _, err = runLifecycleFixture(t, exe, "images", "--json")
	if err != nil || !strings.Contains(out, `"ready":false`) || !strings.Contains(out, "import incomplete") {
		t.Fatalf("catalog hid the image's readiness: %q %v", out, err)
	}
}

func TestUnreadyImageFailsBeforeDiskProvisioning(t *testing.T) {
	unready := strings.Replace(readyImage, `"status":"True"`, `"status":"False"`, 1)
	exe := imageFixture(t, unready, `{"items":[]}`)
	t.Setenv("HARVESTER_IMAGE", "default/image-a")
	out, stderr, err := runLifecycleFixture(t, exe, "create")
	if err == nil || !strings.Contains(stderr, "not ready") || strings.Contains(out, "Provisioning root disk") {
		t.Fatalf("unready image reached disk provisioning: %q %q %v", out, stderr, err)
	}
}

func TestImageCatalogRejectsMalformedAndCrossNamespaceResponses(t *testing.T) {
	for _, payload := range []string{`{`, `{}`, `{"items":[` + strings.Replace(readyImage, `"namespace":"default"`, `"namespace":"other"`, 1) + `]}`} {
		t.Run(payload, func(t *testing.T) {
			exe := imageFixture(t, "", payload)
			_, _, err := runLifecycleFixture(t, exe, "images", "--json")
			if err == nil {
				t.Fatal("invalid catalog accepted")
			}
		})
	}
}
