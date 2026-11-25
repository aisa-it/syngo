package store

import "testing"

// jsonbSafe: NUL в строках и ключах заменяется, остальное не меняется.
func TestJSONBSafe(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		`{"a":"x"}`: `{"a":"x"}`,
		`{"a":"x\u0000y","k\u0000":[1,"\u0000"]}`: `{"a":"x` + "�" + `y","k` + "�" + `":[1,"` + "�" + `"]}`,
		`{"n":12345678901234567890,"h":"<&>"}`:    `{"n":12345678901234567890,"h":"<&>"}`,
		`{"a":"\\u0000"}`:                         `{"a":"\\u0000"}`,
		`not json \u0000`:                         `not json \u0000`,
	}

	for in, want := range cases {
		if got := string(jsonbSafe([]byte(in))); got != want {
			t.Errorf("jsonbSafe(%s) = %s, ожидали %s", in, got, want)
		}
	}
}
