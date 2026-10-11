package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ImmersiveFusion/parallax/internal/plan"
)

// testTimeout is generous on purpose: a 1s budget flaked when the whole suite
// ran in parallel on Windows and a first local connection was slow.
const testTimeout = 5 * time.Second

func TestUnauthorizedAndForbiddenAreAnsweredAndNeedAuth(t *testing.T) {
	setupTracing(t)
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }))
		res := New("t", testTimeout).Do(context.Background(), plan.Call{Node: "svc", Kind: plan.KindHTTP, Method: "GET", URL: srv.URL})
		srv.Close()
		if !res.OK || !res.NeedsAuth {
			t.Fatalf("%d: want answered and needsAuth, got ok=%v needsAuth=%v", code, res.OK, res.NeedsAuth)
		}
	}
}

func TestNonRepeatSafeMethodIsRefusedEvenIfPlanned(t *testing.T) {
	setupTracing(t)
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer srv.Close()
	res := New("t", testTimeout).Do(context.Background(), plan.Call{Node: "svc", Kind: plan.KindHTTP, Method: "POST", URL: srv.URL})
	if res.OK || hit {
		t.Fatal("POST must never be sent")
	}
}

func TestDetailIsTruncatedWithoutSplittingUTF8(t *testing.T) {
	long := strings.Repeat("é", 400) // 800 bytes of two-byte runes
	got := truncate(long, MaxDetail)
	if len(got) > MaxDetail+3 || !strings.HasSuffix(got, "...") {
		t.Fatalf("not truncated: %d bytes", len(got))
	}
	if !strings.HasPrefix(long, strings.TrimSuffix(got, "...")) {
		t.Fatal("truncation changed the kept text")
	}
	if truncate("short", MaxDetail) != "short" {
		t.Fatal("short text must be unchanged")
	}
}

func TestEgressGuard(t *testing.T) {
	for _, a := range []string{"169.254.169.254:80", "[fe80::1]:80", "100.100.100.200:80", "[fd00:ec2::254]:80", "0.0.0.0:80"} {
		if err := guardEgress("tcp", a, nil); err == nil {
			t.Errorf("%s should be refused", a)
		}
	}
	for _, a := range []string{"127.0.0.1:80", "10.0.0.5:443", "[::1]:9090", "93.184.216.34:443"} {
		if err := guardEgress("tcp", a, nil); err != nil {
			t.Errorf("%s should be allowed: %v", a, err)
		}
	}
}

func TestEgressGuardIsAppliedToRealDials(t *testing.T) {
	setupTracing(t)
	call := plan.Call{Node: "md", Kind: plan.KindHTTP, Method: "GET", URL: "http://169.254.169.254/latest/meta-data/"}
	res := New("t", testTimeout).Do(context.Background(), call)
	if res.OK || !strings.Contains(res.Detail, "egress guard") {
		t.Fatalf("the metadata address must be refused by the guard, got ok=%v %q", res.OK, res.Detail)
	}
	// Without dialing: on a cloud CI runner 169.254.169.254 is a live metadata service.
	if New("t", time.Second, WithAllowLinkLocal()).dialer().Control != nil {
		t.Fatal("WithAllowLinkLocal must disable the guard")
	}
	if New("t", time.Second).dialer().Control == nil {
		t.Fatal("the guard must be on by default")
	}
}
