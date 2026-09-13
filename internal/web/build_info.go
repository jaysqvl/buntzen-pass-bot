package web

import (
	"net/url"
	"strconv"
	"strings"
)

type buildDisplay struct {
	Version       string
	Revision      string
	Label         string
	ShortRevision string
	ReleaseURL    string
	CommitURL     string
}

func applicationBuild(version, revision string) buildDisplay {
	build := buildDisplay{Version: version, Revision: revision, Label: "Development build"}
	const repository = "https://github.com/jaysqvl/lake-pass-bot"
	if version != "" && version != "dev" {
		build.Label = "v" + version
		releaseRepository, tagPrefix := repository, "lake-pass-bot-v"
		if historicalRelease(version) {
			// These published tags remain part of the preserved pre-rebrand history.
			releaseRepository, tagPrefix = "https://github.com/jaysqvl/buntzen-pass-bot", "buntzen-pass-bot-v"
		}
		build.ReleaseURL = releaseRepository + "/releases/tag/" + url.PathEscape(tagPrefix+version)
	}
	if revision != "" {
		build.ShortRevision = revision[:min(7, len(revision))]
		build.CommitURL = repository + "/commit/" + url.PathEscape(revision)
	}
	return build
}

func historicalRelease(version string) bool {
	parts := strings.Split(version, ".")
	if len(parts) != 3 || parts[0] != "0" {
		return false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		return false
	}
	return minor >= 0 && patch >= 0 && (minor < 5 || minor == 5 && patch <= 3)
}
