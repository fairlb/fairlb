package config

import (
	"io"
	"strconv"

	foundation "github.com/fairlb/fairlb/foundation/config"
)

var Descriptors = []foundation.Descriptor{
	{Name: "PUBLIC_URL", Product: "community", Type: "url", Default: DefaultPublicURL, Description: "Public origin used for cookies and setup links."},
	{Name: "FAIRLB_DATA_DIR", Product: "community", Type: "path", Default: DefaultDataDir, Description: "Persistent Community state directory."},
	{Name: "FAIRLB_ADMIN_EMAIL", Product: "community", Type: "string", Required: "with FAIRLB_ADMIN_PASSWORD", Description: "Optional first administrator email."},
	{Name: "FAIRLB_ADMIN_PASSWORD", Product: "community", Type: "string", Secret: true, Required: "with FAIRLB_ADMIN_EMAIL", Description: "Optional first administrator password."},
	{Name: "FAIRLB_SETUP_TOKEN", Product: "community", Type: "string", Secret: true, Description: "Optional setup-page token of at least 32 characters."},
	{Name: "FAIRLB_PROBE_TRACE", Product: "community", Type: "bool", Default: "false", Description: "Expose provider routing trace details."},
	{Name: "LOG_FORMAT", Product: "community", Type: "enum", Default: "text", Description: "Log encoding: text or json."},
}

func WriteCheck(w io.Writer, src foundation.Source, cfg Config) error {
	values := cfg.Config.EffectiveValues()
	values["PUBLIC_URL"] = cfg.PublicURL.String()
	values["FAIRLB_DATA_DIR"] = cfg.DataDir
	values["FAIRLB_ADMIN_EMAIL"] = cfg.AdminEmail
	values["FAIRLB_PROBE_TRACE"] = strconv.FormatBool(cfg.ProbeTrace)
	values["LOG_FORMAT"] = cfg.LogFormat
	if len(cfg.SecretKey) == 0 {
		values["SECRET_KEY"] = "<generated and persisted by serve>"
	}
	return foundation.WriteCheck(w, src, append(foundation.SharedDescriptors, Descriptors...), values, map[string]bool{
		"bootstrap_admin": cfg.AdminEmail != "",
		"setup_token":     cfg.SetupToken != "",
		"redis":           cfg.UsesRedis(),
	})
}
