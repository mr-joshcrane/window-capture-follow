package main

import "testing"

func TestMatcherUsesAllConfiguredRules(t *testing.T) {
	match, err := newMatcher(config{
		titlePattern:   `(?i)brood war`,
		classPattern:   `(?i)starcraft\.exe`,
		processPattern: `StarCraft\.exe`,
	})
	if err != nil {
		t.Fatal(err)
	}

	matching := window{title: "Brood War", class: "starcraft.exe", process: `C:\StarCraft.exe -launch`}
	if !match.matches(matching) {
		t.Fatal("expected window to match")
	}
	matching.class = "shieldbattery.exe"
	if match.matches(matching) {
		t.Fatal("expected all configured rules to be required")
	}
}

func TestSelectWindowKeepsCurrentMatch(t *testing.T) {
	match, err := newMatcher(config{titlePattern: `Brood War`})
	if err != nil {
		t.Fatal(err)
	}
	windows := []window{
		{id: 10, title: "Brood War"},
		{id: 20, title: "Brood War"},
	}

	selected, ok := selectWindow(windows, match, 10, 0, windowIdentity{})
	if !ok || selected.id != 10 {
		t.Fatalf("expected current window 10, got %#v", selected)
	}
}

func TestSelectWindowChoosesNewestMatch(t *testing.T) {
	match, err := newMatcher(config{titlePattern: `Brood War`})
	if err != nil {
		t.Fatal(err)
	}
	windows := []window{
		{id: 10, title: "Brood War"},
		{id: 20, title: "Brood War"},
		{id: 30, title: "ShieldBattery"},
	}

	selected, ok := selectWindow(windows, match, 0, 0, windowIdentity{})
	if !ok || selected.id != 20 {
		t.Fatalf("expected newest matching window 20, got %#v", selected)
	}
}

func TestSelectWindowDetectsReusedWindowID(t *testing.T) {
	match, err := newMatcher(config{titlePattern: `Brood War`})
	if err != nil {
		t.Fatal(err)
	}
	windows := []window{{id: 10, pid: 200, title: "Brood War"}}

	selected, ok := selectWindow(windows, match, 10, 100, windowIdentity{})
	if !ok || selected.pid != 200 {
		t.Fatalf("expected replacement process 200, got %#v", selected)
	}
}

func TestSelectWindowExcludesFailedIdentity(t *testing.T) {
	match, err := newMatcher(config{titlePattern: `Brood War`})
	if err != nil {
		t.Fatal(err)
	}
	windows := []window{{id: 10, pid: 100, title: "Brood War"}}

	_, ok := selectWindow(windows, match, 0, 0, windowIdentity{id: 10, pid: 100})
	if ok {
		t.Fatal("expected failed identity to remain excluded")
	}

	windows[0].pid = 200
	selected, ok := selectWindow(windows, match, 0, 0, windowIdentity{id: 10, pid: 100})
	if !ok || selected.pid != 200 {
		t.Fatalf("expected reused ID with new PID to match, got %#v", selected)
	}
}
