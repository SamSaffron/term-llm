package awsauth

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type sharedProfile struct {
	values            map[string]string
	configRegion      string
	credentialsSource string
}

// SharedConfigProfileNotExistError reports a profile explicitly named by the
// application, AWS_PROFILE, or AWS_DEFAULT_PROFILE that appears in neither shared file.
type SharedConfigProfileNotExistError struct{ Profile string }

func (e *SharedConfigProfileNotExistError) Error() string {
	return fmt.Sprintf("AWS shared config profile %q does not exist", e.Profile)
}

func loadSharedProfile(o Options, profile string) (sharedProfile, error) {
	configPath, credentialsPath := sharedFilePaths(o)
	config, err := readSharedINI(configPath, true)
	if err != nil {
		return sharedProfile{}, err
	}
	credentials, err := readSharedINI(credentialsPath, false)
	if err != nil {
		return sharedProfile{}, err
	}
	cv, configExists := config[profile]
	kv, credentialsExist := credentials[profile]
	if !configExists && !credentialsExist && (o.Profile != "" || o.Getenv("AWS_PROFILE") != "" || o.Getenv("AWS_DEFAULT_PROFILE") != "") {
		return sharedProfile{}, &SharedConfigProfileNotExistError{Profile: profile}
	}
	result := sharedProfile{values: make(map[string]string), configRegion: cv["region"], credentialsSource: "shared config file"}
	for k, v := range cv {
		result.values[k] = v
	}
	for k, v := range kv {
		result.values[k] = v
	}
	if kv["aws_access_key_id"] != "" || kv["aws_secret_access_key"] != "" {
		result.credentialsSource = "shared credentials file"
	}
	return result, nil
}

func sharedFilePaths(o Options) (string, string) {
	configPath, credentialsPath := o.Getenv("AWS_CONFIG_FILE"), o.Getenv("AWS_SHARED_CREDENTIALS_FILE")
	home, err := o.HomeDir()
	if err == nil && home != "" {
		if configPath == "" {
			configPath = filepath.Join(home, ".aws", "config")
		}
		if credentialsPath == "" {
			credentialsPath = filepath.Join(home, ".aws", "credentials")
		}
	}
	return configPath, credentialsPath
}

func readSharedINI(path string, isConfig bool) (map[string]map[string]string, error) {
	profiles := make(map[string]map[string]string)
	if path == "" {
		return profiles, nil
	}
	f, err := os.Open(path)
	if err != nil {
		// The SDK treats files which cannot be opened as empty, even when
		// explicitly overridden; do not fall back to another location.
		return profiles, nil
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	var section map[string]string
	nested := false
	for scanner.Scan() {
		raw := scanner.Text()
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			name, ok := sharedSectionName(line, isConfig)
			if !ok {
				// Like the AWS SDK, unrecognized lines are ignored rather than
				// failing the whole file.
				continue
			}
			section = nil
			nested = false
			if name != "" {
				if profiles[name] == nil {
					profiles[name] = make(map[string]string)
				}
				section = profiles[name]
			}
			continue
		}
		// Only indented lines following an empty property belong to a nested
		// service section. The SDK promotes other indented keys to normal keys.
		indented := raw[0] == ' ' || raw[0] == '\t'
		if indented && nested {
			continue
		}
		key, value, ok := splitSharedProperty(line)
		if !ok {
			// Multiline continuations and unrecognized lines are ignored, as
			// in the AWS SDK's lenient parser.
			continue
		}
		value = sharedPropertyValue(value)
		nested = value == ""
		if section != nil {
			section[strings.ToLower(strings.TrimSpace(key))] = value
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read AWS shared file %q: %w", path, err)
	}
	return profiles, nil
}

// sharedSectionName returns the profile name for a section header. ok is false
// for lines the AWS SDK would not recognize as a section header; name is empty
// for recognized non-profile sections (sso-session, services, ...).
func sharedSectionName(line string, isConfig bool) (name string, ok bool) {
	end := strings.IndexByte(line, ']')
	if end < 0 {
		return "", false
	}
	tail := strings.TrimSpace(line[end+1:])
	if tail != "" && !strings.HasPrefix(tail, "#") && !strings.HasPrefix(tail, ";") {
		return "", false
	}
	name = strings.TrimSpace(line[1:end])
	if name == "" {
		return "", true
	}
	if !isConfig || name == "default" {
		return name, true
	}
	parts := strings.Fields(name)
	if len(parts) >= 2 && parts[0] == "profile" {
		return strings.Join(parts[1:], " "), true
	}
	// Non-profile sections (sso-session, services, etc.) are intentionally ignored.
	return "", true
}

func sharedPropertyValue(value string) string {
	// Inline comments must be whitespace-separated; '#' inside a token remains
	// part of it. Like the SDK, outer matching quotes are removed.
	for i := 1; i < len(value); i++ {
		if (value[i] == '#' || value[i] == ';') && (value[i-1] == ' ' || value[i-1] == '\t') {
			value = value[:i]
			break
		}
	}
	return unquoteSharedValue(strings.TrimSpace(value))
}

func unquoteSharedValue(value string) string {
	if len(value) >= 2 && value[0] == value[len(value)-1] && (value[0] == '\'' || value[0] == '"') {
		return value[1 : len(value)-1]
	}
	return value
}

func splitSharedProperty(line string) (string, string, bool) {
	// The SDK also accepts the legacy ':' assignment separator.
	separator := strings.IndexAny(line, "=:")
	if separator < 0 || strings.TrimSpace(line[:separator]) == "" {
		return "", "", false
	}
	return line[:separator], line[separator+1:], true
}
