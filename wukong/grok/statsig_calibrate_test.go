package grok

import "testing"

func liveCalibrationPage(t *testing.T) []byte {
	t.Helper()
	return []byte(`{"curves":` + string(botoxCurvesLiveFixture) + `}`)
}

func TestSolveStatsigByteIndicesRecoversCurrentDefaults(t *testing.T) {
	samples := []StatsigCalibrationSample{
		{"A1Hh9DGzAQ9m9EdXg8PAnvua6mDS9ZSCypbM82jY4v8j/WjW853LN5P/a03gUp50", "cd3d590f851eb851eb8504040f851eb851eb8500"},
		{"AWbgObdGEyo7WF4m/4yCqnaI4VLTqP4YLMykCx+UknImug7mi1RFXNRHqSDNx8oH", "2ad8e30eb851eb851eb8806147ae147ae14806147ae147ae1480eb851eb851eb8800"},
		{"iCWLhcmYw6nDpBUg9Xn6Eqk5f6cNJh2Gu5frGcy/EgoatowZWcNSqSUIH2JO+6WJ", "e7eebf101999999999999a01999999999999a100"},
		{"hd6xqr/3xCIgrwpODdiuN19HFCxsmbrC63CncZlu1U3iyUHhw4bMkefgZQ9DeJYa", "46413b100100"},
		{"tFojyacuwZzEO1fxWg3Yf8RZx+z5Fov5OS9US7E4OQBSvsu5Ux3YHJh8GQ1qBaWX", "33ebef100ccccccccccccd00ccccccccccccd100"},
	}
	solutions, err := SolveStatsigByteIndices(liveCalibrationPage(t), samples)
	if err != nil {
		t.Fatal(err)
	}
	if len(solutions) != 1 {
		t.Fatalf("solutions = %d, want exactly 1: %v", len(solutions), solutions)
	}
	if got, want := solutions[0], CurrentStatsigByteIndices(); got != want {
		t.Fatalf("solved %s, but the compiled default is %s", got, want)
	}
}

func TestSolveStatsigByteIndicesReportsAmbiguityFromOneSample(t *testing.T) {
	samples := []StatsigCalibrationSample{
		{"hd6xqr/3xCIgrwpODdiuN19HFCxsmbrC63CncZlu1U3iyUHhw4bMkefgZQ9DeJYa", "46413b100100"},
	}
	solutions, err := SolveStatsigByteIndices(liveCalibrationPage(t), samples)
	if err != nil {
		t.Fatal(err)
	}
	if len(solutions) < 2 {
		t.Fatalf("solutions = %d, want an ambiguous result", len(solutions))
	}
}

func TestVerifyStatsigByteIndicesPinpointsBadSample(t *testing.T) {
	page := liveCalibrationPage(t)
	good := StatsigCalibrationSample{
		Meta:         "hd6xqr/3xCIgrwpODdiuN19HFCxsmbrC63CncZlu1U3iyUHhw4bMkefgZQ9DeJYa",
		AnimationKey: "46413b100100",
	}

	if bad, err := VerifyStatsigByteIndices(page, []StatsigCalibrationSample{good}, CurrentStatsigByteIndices()); err != nil || bad != -1 {
		t.Fatalf("bad=%d err=%v, want -1", bad, err)
	}

	tampered := good
	tampered.AnimationKey = "deadbeef"
	if bad, err := VerifyStatsigByteIndices(page, []StatsigCalibrationSample{good, tampered}, CurrentStatsigByteIndices()); err != nil || bad != 1 {
		t.Fatalf("bad=%d err=%v, want index 1", bad, err)
	}
}

func TestSolveStatsigByteIndicesRejectsEmptyInput(t *testing.T) {
	if _, err := SolveStatsigByteIndices(liveCalibrationPage(t), nil); err == nil {
		t.Fatal("expected an error when no samples are supplied")
	}
}

func TestStatsigIndexSolutionEquivalentIgnoresTimeOrder(t *testing.T) {
	current := CurrentStatsigByteIndices()
	shuffled := StatsigIndexSolution{SVG: current.SVG, Row: current.Row, TimeA: current.TimeC, TimeB: current.TimeA, TimeC: current.TimeB}
	if !current.Equivalent(shuffled) {
		t.Fatalf("%s should equal %s", current, shuffled)
	}
	other := current
	other.Row++
	if current.Equivalent(other) {
		t.Fatal("different row must not be equivalent")
	}
}
