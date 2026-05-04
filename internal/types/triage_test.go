package types

import "testing"

func TestValidClassifications(t *testing.T) {
	valid := []string{
		"CrashLoop", "OOM", "Network", "ImagePull",
		"ResourceExhaustion", "Config", "Scheduling", "Unknown",
	}
	for _, c := range valid {
		if !ValidClassifications[c] {
			t.Errorf("ValidClassifications[%q] = false, want true", c)
		}
	}
}

func TestValidClassifications_Invalid(t *testing.T) {
	invalid := []string{"crashloop", "oom", "CRASHLOOP", "NotAClass", ""}
	for _, c := range invalid {
		if ValidClassifications[c] {
			t.Errorf("ValidClassifications[%q] = true, want false", c)
		}
	}
}

func TestValidSeverities(t *testing.T) {
	valid := []string{"critical", "warning", "info"}
	for _, s := range valid {
		if !ValidSeverities[s] {
			t.Errorf("ValidSeverities[%q] = false, want true", s)
		}
	}
}

func TestValidSeverities_Invalid(t *testing.T) {
	invalid := []string{"Critical", "CRITICAL", "high", "low", ""}
	for _, s := range invalid {
		if ValidSeverities[s] {
			t.Errorf("ValidSeverities[%q] = true, want false", s)
		}
	}
}
