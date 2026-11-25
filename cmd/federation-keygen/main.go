// Command federation-keygen создаёт ed25519-ключ сервера для федерации.
//
//	federation-keygen -version a_1 -out /etc/syngo/federation.key
//	federation-keygen -version a_1 -out -          (печать ключа в stdout)
//
// Файл формата Synapse: `ed25519 <version> <base64-seed>`. Права выставляются 0600.
// Сделайте backup: без ключа сервер не сможет подписывать федеративные запросы,
// а смена ключа требует публикации old_verify_keys.
//
// Защита идентичности сервера: существующий непустой файл ключа не
// перезаписывается без -force, seed печатается в stdout только по явному -out -.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/aisa-it/syngo/internal/federation/signing"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		log.Fatal("federation-keygen: ", err)
	}
}

func run(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("federation-keygen", flag.ContinueOnError)
	version := fs.String("version", "a_1", "версия ключа (ed25519:<version>)")
	out := fs.String("out", "", "куда записать ключ (обязательно; - печатает ключ в stdout)")
	force := fs.Bool("force", false, "перезаписать существующий ключ (сервер потеряет прежнюю идентичность)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *out == "" {
		return errors.New("укажите -out <файл> (или -out - для печати ключа в stdout)")
	}

	if *out != "-" && !*force {
		if info, err := os.Stat(*out); err == nil && info.Size() > 0 {
			return fmt.Errorf("%s уже существует: перезапись меняет ключ сервера, "+
				"старые подписи станут непроверяемыми; добавьте -force, если это осознанно", *out)
		}
	}

	key, err := signing.Generate(*version)
	if err != nil {
		return err
	}

	content, err := signing.EncodeKeyFile(key)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(stdout, "key_id:     %s\n", key.KeyID)
	_, _ = fmt.Fprintf(stdout, "public_key: %s\n", key.Public())
	_, _ = fmt.Fprintln(stdout, "ВНИМАНИЕ: сделайте backup ключа — потеря означает невозможность подписывать запросы,")
	_, _ = fmt.Fprintln(stdout, "а публикацию нового ключа придётся делать через old_verify_keys.")

	if *out == "-" {
		_, _ = fmt.Fprintln(stdout)
		_, _ = fmt.Fprint(stdout, content)

		return nil
	}

	if err := writeKey(*out, content); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(stdout, "\nключ записан: %s (chmod 600)\n", *out)

	return nil
}

// writeKey пишет ключ и выставляет 0600 и на уже существующем файле: WriteFile
// права существующего файла не меняет (оставался 0644).
func writeKey(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // путь задает оператор
	if err != nil {
		return err
	}

	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()

		return err
	}

	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()

		return err
	}

	return f.Close()
}
