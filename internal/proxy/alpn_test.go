//go:build linux

package proxy

import (
	"reflect"
	"testing"
)

func TestParseALPNMode(t *testing.T) {
	cases := []struct {
		value    string
		wantMode ALPNMode
		wantErr  bool
	}{
		{value: "", wantMode: ALPNModeH1},
		{value: "h1", wantMode: ALPNModeH1},
		{value: "h2", wantMode: ALPNModeH2},
		{value: "h2,h1", wantMode: ALPNModeH2H1},
		{value: "h1,h2", wantErr: true},
		{value: "http2", wantErr: true},
		{value: "h2, h1", wantErr: true},
	}
	for _, test := range cases {
		mode, err := ParseALPNMode(test.value)
		if test.wantErr {
			if err == nil {
				t.Errorf("ParseALPNMode(%q) err = nil, want an error", test.value)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseALPNMode(%q) err = %v", test.value, err)
			continue
		}
		if mode != test.wantMode {
			t.Errorf("ParseALPNMode(%q) = %q, want %q", test.value, mode, test.wantMode)
		}
	}
}

func TestALPNModeProtocols(t *testing.T) {
	cases := []struct {
		mode         ALPNMode
		allowsH2     bool
		client       []string
		wantUpstream []string
	}{
		{ALPNModeH1, false, []string{"http/1.1"}, []string{"http/1.1"}},
		{ALPNModeH2, true, []string{"h2"}, []string{"h2", "http/1.1"}},
		{ALPNModeH2H1, true, []string{"h2", "http/1.1"}, []string{"h2", "http/1.1"}},
	}
	for _, test := range cases {
		if got := test.mode.AllowsH2(); got != test.allowsH2 {
			t.Errorf("%q AllowsH2() = %v, want %v", test.mode, got, test.allowsH2)
		}
		if got := test.mode.ClientProtocols(); !reflect.DeepEqual(got, test.client) {
			t.Errorf("%q ClientProtocols() = %v, want %v", test.mode, got, test.client)
		}
		if got := test.mode.UpstreamProtocols(); !reflect.DeepEqual(got, test.wantUpstream) {
			t.Errorf("%q UpstreamProtocols() = %v, want %v", test.mode, got, test.wantUpstream)
		}
	}
}
