package compose

type runtimeVersionPair struct{ docker, compose string }

// IsVerifiedRuntime reports whether this exact Engine/Compose version pair has
// passed the real Linux compatibility matrix. Probe success alone is not that
// evidence. The production list intentionally starts empty; add an entry only
// together with its real compatibility results and accepted feature boundaries.
// There is no environment/configuration override or version-range fallback.
func IsVerifiedRuntime(dockerVersion, composeVersion string) bool {
	return runtimeVersionListed(dockerVersion, composeVersion, []runtimeVersionPair{
		// No real runtime combination has been verified yet.
	})
}

func runtimeVersionListed(dockerVersion, composeVersion string, evidence []runtimeVersionPair) bool {
	if dockerVersion == "" || composeVersion == "" {
		return false
	}
	for _, pair := range evidence {
		if pair.docker == dockerVersion && pair.compose == composeVersion {
			return true
		}
	}
	return false
}
