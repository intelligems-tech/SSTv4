package aws

import "strings"

// ApplyDevEnvOverrides overlays selected keys of the `sst dev` process
// environment onto the environment a deployed stub forwarded with its Init.
//
// The stub snapshots its own env and sends it along, and the local worker runs
// with exactly that map. Anything a developer wants to change per session,
// such as which database or frontend a handler talks to, would otherwise need
// a redeploy of every stub. `keys` is the comma separated list from
// SST_DEV_ENV_OVERRIDES; only listed keys that are set in `process` are
// applied, and everything else in `forwarded` stays as it was.
func ApplyDevEnvOverrides(forwarded []string, process []string, keys string) []string {
	wanted := map[string]bool{}
	for _, key := range strings.Split(keys, ",") {
		key = strings.TrimSpace(key)
		if key != "" {
			wanted[key] = true
		}
	}
	if len(wanted) == 0 {
		return forwarded
	}
	overrides := map[string]string{}
	for _, entry := range process {
		key, value, ok := strings.Cut(entry, "=")
		if ok && wanted[key] {
			overrides[key] = value
		}
	}
	if len(overrides) == 0 {
		return forwarded
	}
	result := make([]string, 0, len(forwarded)+len(overrides))
	seen := map[string]bool{}
	for _, entry := range forwarded {
		key, _, _ := strings.Cut(entry, "=")
		if value, ok := overrides[key]; ok {
			result = append(result, key+"="+value)
			seen[key] = true
			continue
		}
		result = append(result, entry)
	}
	for key, value := range overrides {
		if !seen[key] {
			result = append(result, key+"="+value)
		}
	}
	return result
}

// envMap turns KEY=VALUE entries into a map; later entries win.
func envMap(entries []string) map[string]string {
	result := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, _ := strings.Cut(entry, "=")
		result[key] = value
	}
	return result
}

// envKeys lists the distinct keys of KEY=VALUE entries, comma separated, in
// first-seen order.
func envKeys(entries []string) string {
	keys := make([]string, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		key, _, _ := strings.Cut(entry, "=")
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	return strings.Join(keys, ",")
}
