package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/identityadapter"
	"github.com/Pinvou/pinvou3/account-platform/internal/securityadmin"
	"golang.org/x/term"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		log.Fatal("usage: account-security <bootstrap|redeem|break-glass>")
	}
	ctx := context.Background()
	directory, err := identityadapter.NewKeycloakDirectory(
		os.Getenv("KEYCLOAK_ADMIN_API_URL"), os.Getenv("KEYCLOAK_PROVISIONER_TOKEN_URL"), os.Getenv("KEYCLOAK_REALM"),
		os.Getenv("KEYCLOAK_PROVISIONER_CLIENT_ID"), os.Getenv("KEYCLOAK_PROVISIONER_CLIENT_SECRET"),
	)
	if err != nil {
		log.Fatal(err)
	}
	manager, err := securityadmin.NewManager(ctx, os.Getenv("DATABASE_URL"), os.Getenv("OIDC_ISSUER"), directory)
	if err != nil {
		log.Fatal(err)
	}
	defer manager.Close()

	switch os.Args[1] {
	case "bootstrap":
		err = bootstrap(ctx, manager, os.Args[2:])
	case "redeem":
		err = redeem(ctx, manager, os.Args[2:])
	case "break-glass":
		verifier, verifierErr := securityadmin.NewBreakGlassCredentialVerifier(os.Getenv("BREAK_GLASS_OPERATOR_PUBLIC_KEY_BASE64URL"))
		if verifierErr != nil {
			err = verifierErr
			break
		}
		manager.WithBreakGlassVerifier(verifier)
		err = breakGlass(ctx, manager, os.Args[2:])
	default:
		err = errors.New("unknown account-security command")
	}
	if err != nil {
		log.Fatal(err)
	}
}

func bootstrap(ctx context.Context, manager *securityadmin.Manager, arguments []string) error {
	flags := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	mode := flags.String("mode", "", "INTERACTIVE_SET_PASSWORD or ONE_TIME_BOOTSTRAP")
	deployment := flags.String("deployment-identity", "", "controlled deployment identity")
	ttl := flags.Duration("ttl", 15*time.Minute, "one-time credential TTL (maximum 60m)")
	passwordFD := flags.Int("password-fd", -1, "controlled descriptor containing two password lines")
	deliveryFD := flags.Int("delivery-fd", -1, "controlled descriptor receiving the one-time credential")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	issue, err := manager.IssueBootstrap(ctx, *deployment, *mode, *ttl)
	if err != nil || issue.Activated {
		if issue.Activated {
			log.Print("super administrator is already active")
		}
		return err
	}
	switch strings.ToUpper(strings.TrimSpace(*mode)) {
	case "INTERACTIVE_SET_PASSWORD":
		password, err := readConfirmedPassword(*passwordFD)
		if err != nil {
			return err
		}
		defer zeroString(&password)
		if err := manager.ActivateInteractive(ctx, *deployment, password); err != nil {
			return err
		}
		log.Print("super administrator activated")
	case "ONE_TIME_BOOTSTRAP":
		if *deliveryFD < 0 {
			return errors.New("--delivery-fd is required; bootstrap credentials are never written to stdout")
		}
		if err := writeSecret(*deliveryFD, issue.Secret); err != nil {
			return err
		}
		zeroString(&issue.Secret)
		if err := manager.MarkDelivered(ctx, issue.CredentialID); err != nil {
			return err
		}
		log.Printf("one-time bootstrap credential generation %d delivered; expires at %s", issue.Generation, issue.ExpiresAt.Format(time.RFC3339))
	}
	return nil
}

func redeem(ctx context.Context, manager *securityadmin.Manager, arguments []string) error {
	flags := flag.NewFlagSet("redeem", flag.ContinueOnError)
	deployment := flags.String("deployment-identity", "", "controlled deployment identity")
	credentialFD := flags.Int("credential-fd", -1, "descriptor containing the one-time credential")
	passwordFD := flags.Int("password-fd", -1, "controlled descriptor containing two formal password lines")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	credential, err := readSingleSecret(*credentialFD)
	if err != nil {
		return err
	}
	defer zeroString(&credential)
	password, err := readConfirmedPassword(*passwordFD)
	if err != nil {
		return err
	}
	defer zeroString(&password)
	if err := manager.Redeem(ctx, *deployment, credential, password); err != nil {
		return err
	}
	log.Print("super administrator activated; bootstrap credential consumed")
	return nil
}

func breakGlass(ctx context.Context, manager *securityadmin.Manager, arguments []string) error {
	flags := flag.NewFlagSet("break-glass", flag.ContinueOnError)
	credentialFD := flags.Int("operator-credential-fd", -1, "descriptor containing the signed operator credential")
	ticket := flags.String("ticket", "", "incident ticket reference")
	reason := flags.String("reason", "", "incident reason (20-2000 characters)")
	evidenceFile := flags.String("evidence-file", "", "incident evidence file")
	confirmation := flags.String("confirm", "", "must equal ALL_SUPER_ADMINS_UNREACHABLE")
	duration := flags.Duration("duration", 30*time.Minute, "emergency access duration (5m-60m)")
	passwordFD := flags.Int("password-fd", -1, "controlled descriptor containing two formal password lines")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if strings.TrimSpace(*evidenceFile) == "" {
		return errors.New("--evidence-file is required")
	}
	operatorCredential, err := readSingleSecret(*credentialFD)
	if err != nil {
		return err
	}
	defer zeroString(&operatorCredential)
	evidence, err := os.ReadFile(*evidenceFile)
	if err != nil {
		return fmt.Errorf("read incident evidence: %w", err)
	}
	password, err := readConfirmedPassword(*passwordFD)
	if err != nil {
		return err
	}
	defer zeroString(&password)
	err = manager.BreakGlass(ctx, securityadmin.BreakGlassRequest{
		OperatorCredential: operatorCredential, IncidentTicket: *ticket, Reason: *reason, Evidence: evidence,
		Confirmation: *confirmation, Duration: *duration, FormalPassword: password,
	})
	for index := range evidence {
		evidence[index] = 0
	}
	if err != nil {
		return err
	}
	log.Print("break-glass recovery activated; critical alert and immutable audit evidence recorded")
	return nil
}

func readConfirmedPassword(fd int) (string, error) {
	if fd < 0 {
		fd = int(os.Stdin.Fd())
	}
	if term.IsTerminal(fd) {
		first, err := term.ReadPassword(fd)
		if err != nil {
			return "", err
		}
		_, _ = fmt.Fprintln(os.Stderr)
		second, err := term.ReadPassword(fd)
		_, _ = fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		if string(first) != string(second) {
			return "", errors.New("password confirmation does not match")
		}
		return string(first), nil
	}
	file := os.NewFile(uintptr(fd), "controlled-password-input")
	if file == nil {
		return "", errors.New("invalid password descriptor")
	}
	scanner := bufio.NewScanner(io.LimitReader(file, 1024))
	values := make([]string, 0, 2)
	for scanner.Scan() && len(values) < 2 {
		values = append(values, strings.TrimSuffix(scanner.Text(), "\r"))
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if len(values) != 2 || values[0] != values[1] {
		return "", errors.New("controlled password descriptor must contain two matching lines")
	}
	return values[0], nil
}

func readSingleSecret(fd int) (string, error) {
	if fd < 0 {
		return "", errors.New("a controlled credential descriptor is required")
	}
	file := os.NewFile(uintptr(fd), "controlled-credential-input")
	if file == nil {
		return "", errors.New("invalid credential descriptor")
	}
	value, err := io.ReadAll(io.LimitReader(file, 4096))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(value)), nil
}

func writeSecret(fd int, value string) error {
	file := os.NewFile(uintptr(fd), "controlled-credential-delivery")
	if file == nil {
		return errors.New("invalid delivery descriptor")
	}
	if _, err := io.WriteString(file, value+"\n"); err != nil {
		return err
	}
	return file.Sync()
}

func zeroString(value *string) {
	if value == nil {
		return
	}
	// Strings are immutable in Go; clearing the last application reference is
	// still useful and avoids accidental later logging or serialization.
	*value = ""
}
