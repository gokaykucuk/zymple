package appdef

import (
	"strings"
	"testing"
)

const imagesAppYAML = "name: shop\ndomains: [shop.example.com]\nweb: { service: web, port: 3000 }\n"

func parseImages(images, compose string) (*App, error) {
	return Parse([]byte(imagesAppYAML+images), []byte(compose))
}

func TestExtraImages(t *testing.T) {
	app, err := parseImages("images: [ocr, pdf_tools]\n",
		"services:\n  web:\n    image: ${IMAGE}\n  ocr:\n    image: ${IMAGE_OCR}\n  pdf:\n    image: ${IMAGE_PDF_TOOLS}\n")
	if err != nil {
		t.Fatal(err)
	}
	if !app.HasImage("ocr") || !app.HasImage("pdf_tools") || app.HasImage("web") || app.HasImage("") {
		t.Errorf("HasImage is wrong for %v", app.Images)
	}
	for service, want := range map[string]string{"web": "", "ocr": "ocr", "pdf": "pdf_tools", "missing": ""} {
		if got := app.Compose.Image(service); got != want {
			t.Errorf("Image(%s) = %q, want %q", service, got, want)
		}
	}

	digests := map[string]string{"": "sha256:" + strings.Repeat("a", 64), "ocr": "sha256:" + strings.Repeat("b", 64), "pdf_tools": "sha256:" + strings.Repeat("c", 64)}
	out, err := app.Render(RenderInput{Release: 4, Digest: digests[""], Port: 20001,
		Images: map[string]string{"ocr": digests["ocr"], "pdf_tools": digests["pdf_tools"]}})
	if err != nil {
		t.Fatal(err)
	}
	doc := string(out)
	for _, name := range []string{"", "ocr", "pdf_tools"} {
		if strings.Count(doc, "image: zymple/shop@"+digests[name]) != 1 {
			t.Errorf("image %q is not used by exactly one service:\n%s", name, doc)
		}
	}
	if strings.Contains(doc, "IMAGE") {
		t.Errorf("a placeholder survived rendering:\n%s", doc)
	}

	// A release that lacks one of the images cannot be rendered.
	if _, err := app.Render(RenderInput{Release: 4, Digest: digests[""], Port: 20001, Images: map[string]string{"ocr": digests["ocr"]}}); err == nil || !strings.Contains(err.Error(), `"pdf_tools"`) {
		t.Errorf("error = %v", err)
	}
}

func TestExtraImagesAreValidated(t *testing.T) {
	const web = "services:\n  web:\n    image: ${IMAGE}\n"
	nine := "images: [a, b, c, d, e, f, g, h, i]\n"
	for name, tc := range map[string]struct{ images, compose, want string }{
		"undeclared":         {"", web + "  ocr:\n    image: ${IMAGE_OCR}\n", "not listed under images"},
		"unused":             {"images: [ocr]\n", web, "no service uses ${IMAGE_OCR}"},
		"web on an extra":    {"images: [ocr]\n", "services:\n  web:\n    image: ${IMAGE_OCR}\n", "the web service must use ${IMAGE}"},
		"upper case name":    {"images: [OCR]\n", web, "image name"},
		"dash in name":       {"images: [my-ocr]\n", web, "image name"},
		"leading digit":      {"images: [1ocr]\n", web, "image name"},
		"empty name":         {"images: [\"\"]\n", web, "image name"},
		"long name":          {"images: [" + strings.Repeat("a", 32) + "]\n", web, "image name"},
		"declared twice":     {"images: [ocr, ocr]\n", web, "declared twice"},
		"too many":           {nine, web, "at most 8 images"},
		"not a list":         {"images: ocr\n", web, "app.yml"},
		"lower placeholder":  {"images: [ocr]\n", web + "  ocr:\n    image: ${IMAGE_ocr}\n", "image must be exactly"},
		"empty placeholder":  {"images: [ocr]\n", web + "  ocr:\n    image: ${IMAGE_}\n", "image must be exactly"},
		"default value":      {"images: [ocr]\n", web + "  ocr:\n    image: ${IMAGE_OCR:-busybox}\n", "image must be exactly"},
		"suffix":             {"images: [ocr]\n", web + "  ocr:\n    image: ${IMAGE_OCR}:latest\n", "image must be exactly"},
		"prefix":             {"images: [ocr]\n", web + "  ocr:\n    image: docker.io/${IMAGE_OCR}\n", "image must be exactly"},
		"plain image":        {"images: [ocr]\n", web + "  ocr:\n    image: busybox\n", "image must be exactly"},
		"two placeholders":   {"images: [ocr]\n", web + "  ocr:\n    image: ${IMAGE_OCR}${IMAGE}\n", "image must be exactly"},
		"placeholder in env": {"images: [ocr]\n", web + "  ocr:\n    image: ${IMAGE_OCR}\n    environment:\n      X: ${IMAGE_OCR}\n", `"$" is not allowed`},
	} {
		_, err := parseImages(tc.images, tc.compose)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want one containing %q", name, err, tc.want)
		}
	}
}

func TestPlaceholderImage(t *testing.T) {
	for in, want := range map[string]string{"${IMAGE}": "", "${IMAGE_OCR}": "ocr", "${IMAGE_A_B9}": "a_b9"} {
		got, ok := PlaceholderImage(in)
		if !ok || got != want {
			t.Errorf("PlaceholderImage(%q) = %q, %v", in, got, ok)
		}
		if want != "" && ImagePlaceholderFor(want) != in {
			t.Errorf("ImagePlaceholderFor(%q) = %q", want, ImagePlaceholderFor(want))
		}
	}
	for _, in := range []string{"", "IMAGE", "$IMAGE", "${IMAGE", "${IMAGE_}", "${IMAGE_ocr}", "${IMAGE_1}", "${IMAGE_OCR}}", " ${IMAGE}",
		"${IMAGE_O-R}", "${IMAGE_" + strings.Repeat("A", 32) + "}", "${IMAGE_ÖCR}", "${image}"} {
		if name, ok := PlaceholderImage(in); ok {
			t.Errorf("PlaceholderImage(%q) = %q, want it refused", in, name)
		}
	}
}

// FuzzPlaceholderImage: only the exact text ImagePlaceholderFor writes may
// be read as a placeholder, whatever a compose file says.
func FuzzPlaceholderImage(f *testing.F) {
	for _, seed := range []string{"${IMAGE}", "${IMAGE_OCR}", "${IMAGE_}", "${IMAGE_ocr}", "busybox", "${IMAGE_OCR:-x}"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		name, ok := PlaceholderImage(s)
		if !ok {
			return
		}
		if name == "" && s != ImagePlaceholder {
			t.Fatalf("PlaceholderImage(%q) read it as the app's image", s)
		}
		if name != "" && (!ValidImageName(name) || ImagePlaceholderFor(name) != s) {
			t.Fatalf("PlaceholderImage(%q) = %q", s, name)
		}
	})
}
