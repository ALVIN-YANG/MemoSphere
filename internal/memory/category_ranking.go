package memory

// Only select expresses a need to apply remembered constraints and preferences.
// Citing a past decision must leave similarly named rules and events on equal terms.
func categoryFactor(purpose, label string) float64 {
	if purpose == "select" {
		switch categoryOf(label).ID {
		case "rule":
			return 1.08
		case "preference":
			return 1.03
		}
	}
	return 1
}
