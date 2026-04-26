package rados

import "log/slog"

type Config struct {
	ConfigFile string
	User       string
	Keyring    string
	Pool       string
	Namespace  string
	Classes    map[string]ClassSpec
	// Logger receives DEBUG lines per RADOS op (read/write/delete) when set.
	Logger *slog.Logger
}
