package pow

import "testing"

// Real accepted challenge captured from the headless browser
// (x-ds-pow-response header + create_pow_challenge response of the same run;
// the completion using answer=103091 returned HTTP 200 with a valid stream).
func TestSolveKnownVector(t *testing.T) {
	ch := Challenge{
		Algorithm:  "DeepSeekHashV1",
		Challenge:  "7ab1ca5d47417ab4c6822a83457acfe520c087ea11abbabae539de4825d86a4e",
		Salt:       "d80decfd6b3295956261",
		Difficulty: 144000,
		Signature:  "211005a24568880fd28e5440e73dda256ada37f26d8040ca21c40930212d05d4",
		ExpireAt:   1789617483879,
	}
	ans, err := Solve(ch)
	if err != nil {
		t.Fatal(err)
	}
	if ans != 103091 {
		t.Fatalf("got answer %d, want 103091", ans)
	}
}
