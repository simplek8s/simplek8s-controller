package nodestate

import (
	"context"
	"testing"

	"github.com/simplek8s/simplek8s-controller/internal/kube"
	"github.com/simplek8s/simplek8s-controller/internal/kubetest"
)

func TestParseUpdate(t *testing.T) {
	cases := []struct {
		name     string
		anns     map[string]string
		present  bool
		value    string
		parseErr bool
		url      string
	}{
		{"absent", map[string]string{}, false, "", false, ""},
		{"present", map[string]string{AnnNextKernel: "202608291203"}, true, "202608291203", false, ""},
		{"invalid", map[string]string{AnnNextKernel: "banana"}, true, "banana", true, ""},
		{"both", map[string]string{AnnNextKernel: "202601010000", AnnUpdateURL: "https://x"}, true, "202601010000", false, "https://x"},
		{"url-only", map[string]string{AnnUpdateURL: "https://y"}, false, "", false, "https://y"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ui := ParseUpdate(tc.anns)
			if ui.NextKernelPresent != tc.present {
				t.Fatalf("present = %v, want %v", ui.NextKernelPresent, tc.present)
			}
			if ui.NextKernel != tc.value {
				t.Fatalf("value = %q, want %q", ui.NextKernel, tc.value)
			}
			if (ui.NextKernelParseErr != "") != tc.parseErr {
				t.Fatalf("parseErr = %q, want set=%v", ui.NextKernelParseErr, tc.parseErr)
			}
			if ui.UpdateURL != tc.url {
				t.Fatalf("url = %q, want %q", ui.UpdateURL, tc.url)
			}
		})
	}
}

func TestPreconds(t *testing.T) {
	absent := &UpdateInfo{}
	set := func(v string) *UpdateInfo { return &UpdateInfo{NextKernelPresent: true, NextKernel: v} }

	if !PrecondAbsent(absent) || PrecondAbsent(set("1")) {
		t.Fatal("PrecondAbsent wrong")
	}
	q := PrecondQuiescent("5")
	if !q(absent) || !q(set("5")) || q(set("6")) {
		t.Fatal("PrecondQuiescent wrong")
	}
	p := PrecondValue("7")
	if p(absent) || p(set("6")) || !p(set("7")) {
		t.Fatal("PrecondValue wrong")
	}
}

func TestNextKernelBuild(t *testing.T) {
	node := &kube.Node{}
	node.Metadata.Name = "n1"
	node.Metadata.ResourceVersion = "42"
	node.Metadata.Annotations = map[string]string{}

	patch, ok := NextKernelBuild("202601010000", PrecondAbsent)(node)
	if !ok {
		t.Fatal("want ok for absent node")
	}
	md := patch["metadata"].(map[string]any)
	if md["resourceVersion"] != "42" {
		t.Fatalf("resourceVersion = %v, want 42", md["resourceVersion"])
	}
	ann := md["annotations"].(map[string]any)
	if ann[AnnNextKernel] != "202601010000" {
		t.Fatalf("annotation = %v", ann[AnnNextKernel])
	}

	node.Metadata.Annotations = map[string]string{AnnNextKernel: "999"}
	if _, ok := NextKernelBuild("202601010000", PrecondAbsent)(node); ok {
		t.Fatal("want abort when annotation present")
	}
}

func TestNextKernelPatchTransition(t *testing.T) {
	f := kubetest.NewFakeAPI()
	t.Cleanup(f.Close)
	f.SetNode("n1", nil, false, nil, "uid-n1")
	creds, err := kubetest.MakeCreds(t)
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.Client(creds.Dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Bootstrap anchor on an absent annotation.
	if err := PatchTransition(ctx, c, "n1", 3, NextKernelBuild("202601010000", PrecondAbsent)); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if got := f.NodeAnnotation("n1", AnnNextKernel); got != "202601010000" {
		t.Fatalf("next-kernel = %q", got)
	}

	// Operator pin is never clobbered: the precondition no longer holds,
	// the transition aborts without writing.
	if err := PatchTransition(ctx, c, "n1", 3, NextKernelBuild("202602020000", PrecondAbsent)); err != ErrAbort {
		t.Fatalf("want ErrAbort, got %v", err)
	}
	if got := f.NodeAnnotation("n1", AnnNextKernel); got != "202601010000" {
		t.Fatalf("operator pin clobbered: %q", got)
	}

	// Staging anchor over a quiescent (== running) value succeeds.
	if err := PatchTransition(ctx, c, "n1", 3, NextKernelBuild("202602020000", PrecondQuiescent("202601010000"))); err != nil {
		t.Fatalf("staging anchor: %v", err)
	}
	if got := f.NodeAnnotation("n1", AnnNextKernel); got != "202602020000" {
		t.Fatalf("next-kernel = %q", got)
	}

	// Staging anchor over a held (!= running) value aborts.
	if err := PatchTransition(ctx, c, "n1", 3, NextKernelBuild("202603030000", PrecondQuiescent("202601010000"))); err != ErrAbort {
		t.Fatalf("want ErrAbort, got %v", err)
	}
	if got := f.NodeAnnotation("n1", AnnNextKernel); got != "202602020000" {
		t.Fatalf("held value clobbered: %q", got)
	}

	// Reset anchors back to running only while the value still matches.
	if err := PatchTransition(ctx, c, "n1", 3, NextKernelBuild("202601010000", PrecondValue("202602020000"))); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if got := f.NodeAnnotation("n1", AnnNextKernel); got != "202601010000" {
		t.Fatalf("next-kernel = %q", got)
	}
}
