package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"unicode"
)

type harvesterImage struct {
	Metadata metadata `json:"metadata"`
	Spec     struct {
		DisplayName string `json:"displayName"`
		Backend     string `json:"backend"`
		URL         string `json:"url"`
	} `json:"spec"`
	Status struct {
		StorageClassName string `json:"storageClassName"`
		Conditions       []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
}

type imageCatalogEntry struct {
	DisplayName string `json:"displayName"`
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	Reference   string `json:"reference"`
	Ready       bool   `json:"ready"`
	Status      string `json:"status"`
}

func imageReadiness(image harvesterImage) string {
	if image.Metadata.DeletionTimestamp != "" {
		return "deleting"
	}
	if image.Status.StorageClassName == "" {
		return "storage class unavailable"
	}
	imported := false
	for _, condition := range image.Status.Conditions {
		if condition.Type == "Imported" && condition.Status == "True" {
			imported = true
		}
		if condition.Type == "RetryLimitExceeded" && condition.Status == "True" {
			return "import retry limit exceeded"
		}
	}
	if !imported {
		return "import incomplete"
	}
	return "ready"
}

func listImages(c config, namespace string) ([]harvesterImage, error) {
	out, err := kubectl(c, "get", "virtualmachineimages", "-n", namespace, "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []harvesterImage `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("decode Harvester image catalog: %w", err)
	}
	if list.Items == nil {
		return nil, errors.New("image catalog response is missing its items list")
	}
	for _, image := range list.Items {
		if image.Metadata.Name == "" || image.Metadata.Namespace != namespace {
			return nil, errors.New("image catalog returned a missing name or unexpected namespace")
		}
	}
	sort.Slice(list.Items, func(i, j int) bool {
		a, b := list.Items[i], list.Items[j]
		displayA, displayB := imageDisplayName(a), imageDisplayName(b)
		if !strings.EqualFold(displayA, displayB) {
			return strings.ToLower(displayA) < strings.ToLower(displayB)
		}
		return a.Metadata.Name < b.Metadata.Name
	})
	return list.Items, nil
}

func imageDisplayName(image harvesterImage) string {
	if image.Spec.DisplayName != "" {
		return image.Spec.DisplayName
	}
	return image.Metadata.Name
}

func imageCatalog(c config) ([]imageCatalogEntry, error) {
	images, err := listImages(c, c.ImageNS)
	if err != nil {
		return nil, err
	}
	entries := make([]imageCatalogEntry, 0, len(images))
	for _, image := range images {
		status := imageReadiness(image)
		entries = append(entries, imageCatalogEntry{imageDisplayName(image), image.Metadata.Namespace, image.Metadata.Name,
			image.Metadata.Namespace + "/" + image.Metadata.Name, status == "ready", status})
	}
	return entries, nil
}

func configuredImage() string {
	if os.Getenv("HARVESTER_IMAGE_SELECTION_MODE") == "catalog" {
		return os.Getenv("HARVESTER_IMAGE_PIN")
	}
	return env("HARVESTER_IMAGE_PIN", os.Getenv("HARVESTER_IMAGE"))
}

func legacyImageSelection(entries []imageCatalogEntry, legacy string) string {
	for _, entry := range entries {
		if entry.Ready && (entry.Reference == legacy || entry.Name == legacy) {
			return entry.Reference
		}
	}
	if strings.Contains(legacy, "/") || strings.HasPrefix(legacy, "image-") {
		return ""
	}
	var matches []string
	for _, entry := range entries {
		if entry.DisplayName == legacy {
			if !entry.Ready {
				return ""
			}
			matches = append(matches, entry.Reference)
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	return ""
}

func printImageOptions(c config, out io.Writer) error {
	entries, catalogErr := imageCatalog(c)
	description := "Select an available image from Harvester."
	pin := env("HARVESTER_IMAGE_PIN", os.Getenv("HARVESTER_IMAGE_CHOICE"))
	if pin == "" && os.Getenv("HARVESTER_IMAGE_SELECTION_MODE") != "catalog" {
		pin = legacyImageSelection(entries, os.Getenv("HARVESTER_IMAGE"))
	}
	if catalogErr != nil {
		description = "Image catalog unavailable. Check the connection, image namespace and image-list permission."
		// Preserve a verified existing selection without exposing resource IDs.
		candidate := pin
		if candidate == "" && os.Getenv("HARVESTER_IMAGE_SELECTION_MODE") != "catalog" {
			candidate = os.Getenv("HARVESTER_IMAGE")
		}
		if ref, err := parseImageReference(candidate, c.ImageNS); err == nil {
			image, err := readImageReference(c, ref)
			if err != nil {
				return fmt.Errorf("cannot verify saved image while catalog is unavailable: %w", err)
			}
			if image != nil && imageReadiness(*image) == "ready" {
				pin = image.Metadata.Namespace + "/" + image.Metadata.Name
				entries = []imageCatalogEntry{{DisplayName: imageDisplayName(*image), Namespace: image.Metadata.Namespace, Name: image.Metadata.Name, Reference: pin, Ready: true, Status: "ready"}}
			}
		}
	}
	type choice struct {
		Value       string `json:"value"`
		DisplayName string `json:"displayName"`
	}
	choices := []choice{{"", "Select an image"}}
	counts := map[string]int{}
	for _, entry := range entries {
		if entry.Ready {
			counts[entry.DisplayName]++
		}
	}
	selected := ""
	for _, entry := range entries {
		if !entry.Ready {
			continue
		}
		if counts[entry.DisplayName] > 1 {
			description += " Images with duplicate names must be renamed in Harvester before selecting them."
			if entry.Reference != pin {
				continue
			}
		}
		choices = append(choices, choice{entry.Reference, entry.DisplayName})
		if entry.Reference == pin {
			selected = pin
		}
	}
	if pin != "" && selected == "" && catalogErr == nil {
		description = "The previously selected image is unavailable. Select an available image from Harvester."
	}
	return json.NewEncoder(out).Encode(map[string]any{"options": map[string]any{
		"HARVESTER_IMAGE_CHOICE": map[string]any{
			"displayName": "Image", "description": description,
			"required": false, "default": selected, "enum": choices,
		},
	}})
}

func printImages(c config, asJSON bool, out io.Writer) error {
	entries, err := imageCatalog(c)
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(out).Encode(entries)
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "DISPLAY NAME\tREFERENCE\tSTATUS"); err != nil {
		return err
	}
	for _, entry := range entries {
		name := strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return ' '
			}
			return r
		}, entry.DisplayName)
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\n", name, entry.Reference, entry.Status); err != nil {
			return err
		}
	}
	return w.Flush()
}

var kubernetesName = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

func readImageReference(c config, ref imageInfo) (*harvesterImage, error) {
	if !kubernetesName.MatchString(ref.Name) {
		return nil, nil
	}
	out, err := kubectl(c, "get", "virtualmachineimage", ref.Name, "-n", ref.Namespace, "-o", "json", "--ignore-not-found")
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil
	}
	var image harvesterImage
	if err := json.Unmarshal(out, &image); err != nil {
		return nil, fmt.Errorf("decode Harvester image: %w", err)
	}
	if image.Metadata.Name != ref.Name || image.Metadata.Namespace != ref.Namespace {
		return nil, errors.New("image lookup returned an unexpected resource identity")
	}
	return &image, nil
}

func resolveImage(c config) (imageInfo, error) {
	ref, err := parseImageReference(c.Image, c.ImageNS)
	if err != nil {
		return imageInfo{}, err
	}
	selected, err := readImageReference(c, ref)
	if err != nil {
		return imageInfo{}, err
	}
	// Namespaced references and Harvester-generated IDs are stable identifiers.
	// Never reinterpret a deleted ID as another image's display name.
	if selected == nil && (strings.Contains(c.Image, "/") || strings.HasPrefix(ref.Name, "image-")) {
		return imageInfo{}, fmt.Errorf("harvester image %s/%s not found", ref.Namespace, ref.Name)
	}
	if selected == nil {
		images, err := listImages(c, ref.Namespace)
		if err != nil {
			return imageInfo{}, err
		}
		var matches []string
		for i := range images {
			if images[i].Spec.DisplayName == c.Image {
				selected = &images[i]
				matches = append(matches, images[i].Metadata.Namespace+"/"+images[i].Metadata.Name)
			}
		}
		if len(matches) > 1 {
			return imageInfo{}, fmt.Errorf("image display name %q is ambiguous; use an exact reference: %s", c.Image, strings.Join(matches, ", "))
		}
	}
	if selected == nil {
		return imageInfo{}, fmt.Errorf("harvester image %q not found in namespace %s; list images to choose a reference", c.Image, ref.Namespace)
	}
	if status := imageReadiness(*selected); status != "ready" {
		return imageInfo{}, fmt.Errorf("harvester image %s/%s is not ready: %s", ref.Namespace, selected.Metadata.Name, status)
	}
	image := imageInfo{Namespace: ref.Namespace, Name: selected.Metadata.Name, StorageClass: selected.Status.StorageClassName, Format: c.ImageType}
	if selected.Spec.Backend == "cdi" && image.Format == "raw" && strings.HasSuffix(strings.ToLower(selected.Spec.URL), ".iso") {
		image.Format = "iso"
	}
	return image, nil
}
