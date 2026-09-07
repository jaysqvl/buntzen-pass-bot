package web

import "net/url"

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
	const repository = "https://github.com/jaysqvl/buntzen-pass-bot"
	if version != "" && version != "dev" {
		build.Label = "v" + version
		build.ReleaseURL = repository + "/releases/tag/" + url.PathEscape("buntzen-pass-bot-v"+version)
	}
	if revision != "" {
		build.ShortRevision = revision[:min(7, len(revision))]
		build.CommitURL = repository + "/commit/" + url.PathEscape(revision)
	}
	return build
}
