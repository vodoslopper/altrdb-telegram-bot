package config

// Build identity, overridden at link time:
//
//	go build -ldflags "-X altrdb-telegram-bot/internal/config.Version=1.2.3 -X altrdb-telegram-bot/internal/config.Commit=abcdef"
var (
	// Version is the release version; "dev" for an untagged build.
	Version = "dev"
	// Commit is the source commit the binary was built from.
	Commit = ""
)
