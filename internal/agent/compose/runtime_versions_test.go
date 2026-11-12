package compose

import "testing"

func TestProductionRuntimeListStartsClosed(t *testing.T) {
	for _, pair := range [][2]string{{"", ""}, {"27.0", "v2.test"}, {"27.0.1", "2.31.0"}, {"999.999", "v999.999"}, {"27.0", " v2.test "}} {
		if IsVerifiedRuntime(pair[0], pair[1]) {
			t.Fatal("unverified runtime admitted")
		}
	}
}

func TestPrivateFixtureRuntimeListMatchesOnlyExactPair(t *testing.T) {
	fixture := []runtimeVersionPair{{docker: "27.0-fixture", compose: "v2.fixture"}}
	for _, pair := range [][2]string{{"27.0-fixture", "v2.fixture "}, {"27.0-fixture", "2.fixture"}, {"27.0-fixture", "v2.fixture-newer"}, {"28.0-fixture", "v2.fixture"}, {"", ""}} {
		if runtimeVersionListed(pair[0], pair[1], fixture) {
			t.Fatal("runtime matched without exact evidence")
		}
	}
	if !runtimeVersionListed("27.0-fixture", "v2.fixture", fixture) {
		t.Fatal("private exact fixture did not match")
	}
	if IsVerifiedRuntime("27.0-fixture", "v2.fixture") {
		t.Fatal("private fixture leaked into production allowlist")
	}
}
