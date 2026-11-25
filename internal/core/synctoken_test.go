package core_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

func TestSyncTokenRoundTrip(t *testing.T) {
	t.Parallel()

	codec := core.NewSyncTokenCodec("test.local", []byte("secret"))

	for _, position := range []int64{0, 1, 42, 1 << 40} {
		token := codec.Encode(position)
		if !strings.HasPrefix(token, "v1.e") {
			t.Fatalf("Encode(%d) = %q, ожидали префикс v1.e", position, token)
		}

		decoded, err := codec.Decode(token)
		if err != nil {
			t.Fatalf("Decode(%q) error: %v", token, err)
		}

		if decoded != position {
			t.Fatalf("Decode(%q) = %d, ожидали %d", token, decoded, position)
		}
	}
}

func TestSyncTokenRejectsTampering(t *testing.T) {
	t.Parallel()

	codec := core.NewSyncTokenCodec("test.local", []byte("secret"))
	token := codec.Encode(10)

	// Подпись отделена последней точкой: подменяем только позицию, подпись оставляем.
	mac := token[strings.LastIndex(token, "."):]

	cases := []string{
		"",
		"v1.e10",         // без подписи
		"v1.e11" + mac,   // позиция подменена
		token + "x",      // хвост
		"v2" + token[2:], // другая версия
		"garbage",
		strings.Repeat("a", 300), // длиннее лимита
	}

	for _, broken := range cases {
		if _, err := codec.Decode(broken); !errors.Is(err, core.ErrUnknownPos) {
			t.Fatalf("Decode(%q) = %v, ожидали ErrUnknownPos", broken, err)
		}
	}
}

func TestSyncTokenBindsServerNameAndSecret(t *testing.T) {
	t.Parallel()

	first := core.NewSyncTokenCodec("one.local", []byte("secret"))
	token := first.Encode(7)

	if _, err := core.NewSyncTokenCodec("two.local", []byte("secret")).Decode(token); !errors.Is(err, core.ErrUnknownPos) {
		t.Fatalf("токен чужого сервера принят: %v", err)
	}

	if _, err := core.NewSyncTokenCodec("one.local", []byte("other")).Decode(token); !errors.Is(err, core.ErrUnknownPos) {
		t.Fatalf("токен с другим секретом принят: %v", err)
	}

	if decoded, err := core.NewSyncTokenCodec("one.local", []byte("secret")).Decode(token); err != nil || decoded != 7 {
		t.Fatalf("свой токен не принят: %d, %v", decoded, err)
	}
}

// TestSyncTokenToDeviceBookmark — токен несёт две независимые позиции: события и
// закладку to-device. Одно число не работало: при очереди больше пачки next_batch
// опускался и таймлайн повторялся.
func TestSyncTokenToDeviceBookmark(t *testing.T) {
	t.Parallel()

	codec := core.NewSyncTokenCodec("test.local", []byte("secret"))

	token := codec.Encode2(42, 7)

	position, toDevice, err := codec.Decode2(token)
	if err != nil {
		t.Fatalf("Decode2() error: %v", err)
	}

	if position != 42 || toDevice != 7 {
		t.Fatalf("Decode2() = %d,%d, ожидали 42,7", position, toDevice)
	}

	// Старый токен без закладки читается как to-device = 0, позиция событий та же.
	legacy := codec.Encode(42)

	position, toDevice, err = codec.Decode2(legacy)
	if err != nil {
		t.Fatalf("Decode2(legacy) error: %v", err)
	}

	if position != 42 || toDevice != 0 {
		t.Fatalf("legacy Decode2() = %d,%d, ожидали 42,0", position, toDevice)
	}

	// Подпись покрывает обе компоненты: подмена закладки невалидна.
	tampered := token[:len(token)-1]
	if tampered[len(tampered)-1] == '0' {
		tampered = tampered[:len(tampered)-1] + "1"
	} else {
		tampered = tampered[:len(tampered)-1] + "0"
	}

	if _, _, err := codec.Decode2(tampered); err == nil {
		t.Fatal("подмена закладки не отвергнута")
	}
}

// TestIsSynapseSyncToken - since-токены Synapse узнаются (initial sync после
// переезда), в том числе с multi-writer ключами в середине;
// наши, токены пагинации и мусор - нет.
func TestIsSynapseSyncToken(t *testing.T) {
	t.Parallel()

	codec := core.NewSyncTokenCodec("test.local", []byte("secret"))

	for _, token := range []string{
		"s72594_4483_1934_0_1_1_1_1_0_1", "m1234~1.1230~2.1234_5_0_1_1_1_1_0_1", "s5",
		// Multi-writer ключи внутри StreamToken (MultiWriterStreamToken Synapse:
		// receipts, account data, to-device, device lists и др. при нескольких
		// writer).
		"s123_4_5_m10~1.10~2.9_0_1_1_1_1_0_1",
		"m50~1.50~3.48_7_m12~1.12_0_1_m3~2.3_1_1_1_0_1",
	} {
		if !core.IsSynapseSyncToken(token) {
			t.Errorf("IsSynapseSyncToken(%q) = false", token)
		}
	}

	for _, token := range []string{
		"", codec.Encode(5), codec.Encode3(5, 3, 7), "s", "sx_1", "s1_1; drop", "garbage",
		"s1_m", "s1__2", "m1~1", "s1_m2~x.1", "s1_2_", "t5-10_1",
	} {
		if core.IsSynapseSyncToken(token) {
			t.Errorf("IsSynapseSyncToken(%q) = true", token)
		}
	}
}

// TestSyncTokenEphemeralVersion - версия эфемерного потока в токене:
// круговой разбор, старые токены без нее читаются с -1, подделка отбивается.
func TestSyncTokenEphemeralVersion(t *testing.T) {
	t.Parallel()

	codec := core.NewSyncTokenCodec("test.local", []byte("secret"))

	for _, tc := range []struct{ pos, toDevice, eph int64 }{
		{10, 0, 1795214358105661}, {10, 7, 5}, {0, 0, 0}, {42, 3, -1},
	} {
		token := codec.Encode3(tc.pos, tc.toDevice, tc.eph)

		pos, toDevice, eph, err := codec.Decode3(token)
		if err != nil || pos != tc.pos || toDevice != tc.toDevice || eph != tc.eph {
			t.Fatalf("Decode3(%q) = %d %d %d %v, ожидали %+v", token, pos, toDevice, eph, err, tc)
		}

		// Старый разбор тоже читает новый токен.
		if pos2, toDevice2, err := codec.Decode2(token); err != nil || pos2 != tc.pos || toDevice2 != tc.toDevice {
			t.Fatalf("Decode2(%q) = %d %d %v", token, pos2, toDevice2, err)
		}
	}

	// Токен старого формата: версии нет.
	if _, _, eph, err := codec.Decode3(codec.Encode2(5, 2)); err != nil || eph != -1 {
		t.Fatalf("старый токен: eph %d, %v; ожидали -1", eph, err)
	}

	token := codec.Encode3(5, 0, 100)
	if _, _, _, err := codec.Decode3(strings.Replace(token, ".r100", ".r999", 1)); err == nil {
		t.Fatal("подмененная версия принята")
	}
}
