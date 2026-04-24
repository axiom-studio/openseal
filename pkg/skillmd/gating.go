package skillmd

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// AvailabilityResult reports whether a skill is available and why not.
type AvailabilityResult struct {
	IsAvailable bool
	Reasons     []string
	MissingEnv  []string
	MissingBins []string
}

// CheckAvailability determines whether a parsed SKILL.md is available on the current system.
// Checks are performed in order: OS → bins → anyBins → env → config.
func CheckAvailability(skill *ParsedSkill, config map[string]interface{}) *AvailabilityResult {
	result := &AvailabilityResult{IsAvailable: true}

	if skill == nil {
		return result
	}

	meta := skill.Metadata

	if meta.Always {
		return result
	}

	if !checkOS(meta.OS, result) {
		return result
	}

	checkBins(meta.RequiresBins, result)
	if !result.IsAvailable {
		return result
	}

	checkAnyBins(meta.RequiresAnyBin, result)
	if !result.IsAvailable {
		return result
	}

	checkEnv(meta.RequiresEnv, result)
	if !result.IsAvailable {
		return result
	}

	checkConfig(meta.RequiresConfig, config, result)
	return result
}

func checkOS(osList []string, result *AvailabilityResult) bool {
	if len(osList) == 0 {
		return true
	}
	for _, o := range osList {
		if strings.EqualFold(o, runtime.GOOS) {
			return true
		}
	}
	result.IsAvailable = false
	result.Reasons = append(result.Reasons, "unsupported os: "+runtime.GOOS)
	return false
}

func checkBins(bins []string, result *AvailabilityResult) {
	for _, bin := range bins {
		if _, err := exec.LookPath(bin); err != nil {
			result.IsAvailable = false
			result.MissingBins = append(result.MissingBins, bin)
			result.Reasons = append(result.Reasons, "missing bin: "+bin)
		}
	}
}

func checkAnyBins(anyBins []string, result *AvailabilityResult) {
	if len(anyBins) == 0 {
		return
	}
	for _, bin := range anyBins {
		if _, err := exec.LookPath(bin); err == nil {
			return
		}
	}
	result.IsAvailable = false
	result.Reasons = append(result.Reasons, "no bin found in anyBins: "+strings.Join(anyBins, ", "))
	result.MissingBins = append(result.MissingBins, anyBins...)
}

func checkEnv(envVars []string, result *AvailabilityResult) {
	for _, env := range envVars {
		if os.Getenv(env) == "" {
			result.IsAvailable = false
			result.MissingEnv = append(result.MissingEnv, env)
			result.Reasons = append(result.Reasons, "missing env: "+env)
		}
	}
}

func checkConfig(keys []string, config map[string]interface{}, result *AvailabilityResult) {
	for _, key := range keys {
		val, ok := lookupConfigKey(config, key)
		if !ok || !isTruthy(val) {
			result.IsAvailable = false
			result.Reasons = append(result.Reasons, "missing or falsy config: "+key)
		}
	}
}

func lookupConfigKey(config map[string]interface{}, key string) (interface{}, bool) {
	parts := strings.Split(key, ".")
	var current interface{} = config
	for _, part := range parts {
		m, ok := current.(map[string]interface{})
		if !ok {
			return nil, false
		}
		current, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func isTruthy(val interface{}) bool {
	if val == nil {
		return false
	}
	switch v := val.(type) {
	case bool:
		return v
	case string:
		return v != ""
	case int:
		return v != 0
	case int64:
		return v != 0
	case float64:
		return v != 0
	default:
		return true
	}
}
