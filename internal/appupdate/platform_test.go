package appupdate

import "testing"

func TestPlatformReleaseNeverInstallsAnotherPlatform(t *testing.T) {
	r := fixtureRelease("0.2.0", false)
	windowsAsset := r.Assets[0]
	windowsAsset.Name = "Cage-0.2.0-windows-amd64-setup.exe"
	windowsAsset.URL = "https://github.com/" + repository + "/releases/download/v0.2.0/" + windowsAsset.Name
	r.Assets = append(r.Assets, windowsAsset)
	for _, tc := range []struct{ os, arch, name string }{
		{"windows", "amd64", windowsAsset.Name},
		{"darwin", "arm64", r.Assets[0].Name},
	} {
		c, err := selectPlatformRelease([]release{r}, "0.1.0", tc.os, tc.arch)
		if err != nil || c == nil || c.Archive.Name != tc.name {
			t.Fatalf("%s/%s: %#v %v", tc.os, tc.arch, c, err)
		}
	}
	if c, err := selectPlatformRelease([]release{fixtureRelease("0.2.0", false)}, "0.1.0", "windows", "amd64"); err != nil || c != nil {
		t.Fatalf("macOS package selected on Windows: %#v %v", c, err)
	}
	// An Intel Mac never installs the Apple Silicon package, and vice versa.
	if c, err := selectPlatformRelease([]release{r}, "0.1.0", "darwin", "amd64"); err != nil || c != nil {
		t.Fatalf("Apple Silicon package selected on an Intel Mac: %#v %v", c, err)
	}
	intel := r.Assets[0]
	intel.Name = "Cage-0.2.0-macos-amd64.dmg"
	intel.URL = "https://github.com/" + repository + "/releases/download/v0.2.0/" + intel.Name
	r.Assets = append(r.Assets, intel)
	for _, tc := range []struct{ arch, name string }{{"amd64", intel.Name}, {"arm64", r.Assets[0].Name}} {
		if c, err := selectPlatformRelease([]release{r}, "0.1.0", "darwin", tc.arch); err != nil || c == nil || c.Archive.Name != tc.name {
			t.Fatalf("darwin/%s: %#v %v", tc.arch, c, err)
		}
	}
	if _, err := selectPlatformRelease([]release{r}, "0.1.0", "windows", "arm64"); err == nil {
		t.Fatal("unsupported architecture appeared up to date")
	}
}
