package model

import "testing"

func TestProfileBrowserSelectionBoundary(t *testing.T) {
	p := Profile{Name: "Example", DefaultVehicle: "Car", OTPSourceID: 1, DefaultTimeoutMS: 1000, LoginProbeURL: "https://example.test/login"}
	for _, channel := range []string{"", "chrome", "chrome-beta", "chrome-dev", "chrome-canary", " CHROME "} {
		p.BrowserChannel = channel
		if err := p.Validate(); err != nil {
			t.Errorf("supported channel %q: %v", channel, err)
		}
	}
	p.BrowserChannel = ""
	for _, path := range []string{"/tmp/member-program", "google-chrome", "../browser", " /Applications/Custom Chrome "} {
		p.BrowserExecutable = path
		if err := p.Validate(); err == nil {
			t.Errorf("accepted member executable %q", path)
		}
	}
	p.BrowserExecutable = ""
	for _, channel := range []string{"/tmp/chrome", "chromium;id", "firefox", "chrome\x00"} {
		p.BrowserChannel = channel
		if err := p.Validate(); err == nil {
			t.Errorf("accepted unsupported channel %q", channel)
		}
	}
}
