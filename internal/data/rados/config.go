package rados

type Config struct {
	ConfigFile string
	User       string
	Keyring    string
	Pool       string
	Namespace  string
	Classes    map[string]ClassSpec
}
