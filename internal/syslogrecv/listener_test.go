package syslogrecv

import (
	"net"
	"testing"
)

func TestParse_RFC3164_Basic(t *testing.T) {
	raw := `<13>Oct 11 22:14:15 mymachine su: 'su root' failed for lonvick`
	msg, err := Parse(raw, "192.168.1.1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Host != "mymachine" {
		t.Errorf("Host = %q, want %q", msg.Host, "mymachine")
	}
	if msg.Tag != "su" {
		t.Errorf("Tag = %q, want %q", msg.Tag, "su")
	}
	if msg.Message != "'su root' failed for lonvick" {
		t.Errorf("Message = %q, want %q", msg.Message, "'su root' failed for lonvick")
	}
	if msg.Severity != "info" { // pri 13 = facility 1, severity 5 (notice) -> info
		t.Errorf("Severity = %q, want %q", msg.Severity, "info")
	}
	if msg.Facility != 1 {
		t.Errorf("Facility = %d, want %d", msg.Facility, 1)
	}
}

func TestParse_RFC3164_NoTag(t *testing.T) {
	raw := `<14>Oct 11 22:14:15 router link down on eth0`
	msg, err := Parse(raw, "192.168.1.1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Host != "router" {
		t.Errorf("Host = %q, want %q", msg.Host, "router")
	}
	if msg.Tag != "-" {
		t.Errorf("Tag = %q, want %q (no tag present)", msg.Tag, "-")
	}
	if msg.Message != "link down on eth0" {
		t.Errorf("Message = %q, want %q", msg.Message, "link down on eth0")
	}
}

func TestParse_RFC5424_WithStructuredData(t *testing.T) {
	// This is the exact real-world payload that used to leak structured
	// data into the message text - captured from `logger` (util-linux)
	// during manual testing.
	raw := `<13>1 2026-07-26T22:00:15.786557+00:00 vm nginx - - [timeQuality tzKnown="1" isSynced="0"] GET /index.html -> 200`
	msg, err := Parse(raw, "127.0.0.1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Host != "vm" {
		t.Errorf("Host = %q, want %q", msg.Host, "vm")
	}
	if msg.Tag != "nginx" {
		t.Errorf("Tag = %q, want %q", msg.Tag, "nginx")
	}
	want := "GET /index.html -> 200"
	if msg.Message != want {
		t.Errorf("Message = %q, want %q (structured-data leaked into message)", msg.Message, want)
	}
}

func TestParse_RFC5424_NoStructuredData(t *testing.T) {
	// The bare "-" case that used to panic with "index out of range" in
	// skipStructuredData before it was fixed. This specifically requires
	// the structured-data field to be the *exact last byte* of the
	// message (nothing trailing it) - that's what makes it a single
	// 1-character "-" string reaching skipStructuredData, which is the
	// exact input that indexed out of bounds on s[2:].
	raw := `<14>1 2026-01-01T00:00:00Z host tag - - -`
	msg, err := Parse(raw, "127.0.0.1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Message != "" {
		t.Errorf("Message = %q, want empty string", msg.Message)
	}
}

func TestParse_RFC5424_NoStructuredDataWithMessage(t *testing.T) {
	raw := `<14>1 2026-01-01T00:00:00Z host tag - - - hello world`
	msg, err := Parse(raw, "127.0.0.1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Message != "hello world" {
		t.Errorf("Message = %q, want %q", msg.Message, "hello world")
	}
}

func TestParse_RFC5424_MultipleStructuredDataBlocks(t *testing.T) {
	raw := `<13>1 2026-01-01T00:00:00Z host tag - - [a x="1"][b y="2"] the actual message`
	msg, err := Parse(raw, "127.0.0.1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Message != "the actual message" {
		t.Errorf("Message = %q, want %q", msg.Message, "the actual message")
	}
}

func TestParse_MalformedPriority(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"no leading angle bracket", "no priority here"},
		{"unclosed angle bracket", "<13 missing close"},
		{"non-numeric priority", "<abc>message"},
		{"empty priority", "<>message"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.raw, "127.0.0.1")
			if err == nil {
				t.Errorf("expected an error for input %q, got nil", tt.raw)
			}
		})
	}
}

func TestParse_SeverityMapping(t *testing.T) {
	tests := []struct {
		pri     int
		wantSev string
		wantFac int
	}{
		{0, "error", 0},
		{3, "error", 0},
		{4, "warn", 0},
		{5, "info", 0},
		{7, "debug", 0},
		{131, "error", 16}, // facility 16 (local0), severity 3 - what `logger -p local0.err` sends
	}
	for _, tt := range tests {
		raw := "<" + itoa(tt.pri) + ">Oct 11 22:14:15 host tag: msg"
		msg, err := Parse(raw, "127.0.0.1")
		if err != nil {
			t.Fatalf("pri=%d: unexpected error: %v", tt.pri, err)
		}
		if msg.Severity != tt.wantSev {
			t.Errorf("pri=%d: Severity = %q, want %q", tt.pri, msg.Severity, tt.wantSev)
		}
		if msg.Facility != tt.wantFac {
			t.Errorf("pri=%d: Facility = %d, want %d", tt.pri, msg.Facility, tt.wantFac)
		}
	}
}

func TestParse_FallsBackToSourceIPWhenNoHost(t *testing.T) {
	raw := `<13>justsomething`
	msg, err := Parse(raw, "10.0.0.5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Host != "10.0.0.5" {
		t.Errorf("Host = %q, want fallback to source IP %q", msg.Host, "10.0.0.5")
	}
}

func TestParseCIDRs_BareIPBecomesSlash32(t *testing.T) {
	nets, err := parseCIDRs([]string{"192.168.1.5"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nets) != 1 {
		t.Fatalf("expected 1 net, got %d", len(nets))
	}
	if ones, _ := nets[0].Mask.Size(); ones != 32 {
		t.Errorf("expected /32 mask for a bare IP, got /%d", ones)
	}
}

func TestParseCIDRs_InvalidEntry(t *testing.T) {
	_, err := parseCIDRs([]string{"not-an-ip-or-cidr"})
	if err == nil {
		t.Error("expected an error for an invalid CIDR entry, got nil")
	}
}

func TestSourceAllowed(t *testing.T) {
	nets, err := parseCIDRs([]string{"192.168.1.0/24"})
	if err != nil {
		t.Fatal(err)
	}

	allowed := &net.UDPAddr{IP: net.ParseIP("192.168.1.42"), Port: 12345}
	if !sourceAllowed(allowed, nets) {
		t.Error("expected 192.168.1.42 to be allowed under 192.168.1.0/24")
	}

	blocked := &net.UDPAddr{IP: net.ParseIP("10.0.0.1"), Port: 12345}
	if sourceAllowed(blocked, nets) {
		t.Error("expected 10.0.0.1 to be blocked - it's outside 192.168.1.0/24")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}
