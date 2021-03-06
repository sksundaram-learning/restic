package main

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"runtime"
	"strings"
	"syscall"

	"github.com/restic/restic/internal/backend/azure"
	"github.com/restic/restic/internal/backend/b2"
	"github.com/restic/restic/internal/backend/gs"
	"github.com/restic/restic/internal/backend/local"
	"github.com/restic/restic/internal/backend/location"
	"github.com/restic/restic/internal/backend/rest"
	"github.com/restic/restic/internal/backend/s3"
	"github.com/restic/restic/internal/backend/sftp"
	"github.com/restic/restic/internal/backend/swift"
	"github.com/restic/restic/internal/debug"
	"github.com/restic/restic/internal/options"
	"github.com/restic/restic/internal/repository"
	"github.com/restic/restic/internal/restic"

	"github.com/restic/restic/internal/errors"

	"golang.org/x/crypto/ssh/terminal"
)

var version = "compiled manually"

// GlobalOptions hold all global options for restic.
type GlobalOptions struct {
	Repo         string
	PasswordFile string
	Quiet        bool
	NoLock       bool
	JSON         bool

	ctx      context.Context
	password string
	stdout   io.Writer
	stderr   io.Writer

	Options []string

	extended options.Options
}

var globalOptions = GlobalOptions{
	stdout: os.Stdout,
	stderr: os.Stderr,
}

func init() {
	var cancel context.CancelFunc
	globalOptions.ctx, cancel = context.WithCancel(context.Background())
	AddCleanupHandler(func() error {
		cancel()
		return nil
	})

	f := cmdRoot.PersistentFlags()
	f.StringVarP(&globalOptions.Repo, "repo", "r", os.Getenv("RESTIC_REPOSITORY"), "repository to backup to or restore from (default: $RESTIC_REPOSITORY)")
	f.StringVarP(&globalOptions.PasswordFile, "password-file", "p", os.Getenv("RESTIC_PASSWORD_FILE"), "read the repository password from a file (default: $RESTIC_PASSWORD_FILE)")
	f.BoolVarP(&globalOptions.Quiet, "quiet", "q", false, "do not output comprehensive progress report")
	f.BoolVar(&globalOptions.NoLock, "no-lock", false, "do not lock the repo, this allows some operations on read-only repos")
	f.BoolVarP(&globalOptions.JSON, "json", "", false, "set output mode to JSON for commands that support it")

	f.StringSliceVarP(&globalOptions.Options, "option", "o", []string{}, "set extended option (`key=value`, can be specified multiple times)")

	restoreTerminal()
}

// checkErrno returns nil when err is set to syscall.Errno(0), since this is no
// error condition.
func checkErrno(err error) error {
	e, ok := err.(syscall.Errno)
	if !ok {
		return err
	}

	if e == 0 {
		return nil
	}

	return err
}

func stdinIsTerminal() bool {
	return terminal.IsTerminal(int(os.Stdin.Fd()))
}

func stdoutIsTerminal() bool {
	return terminal.IsTerminal(int(os.Stdout.Fd()))
}

func stdoutTerminalWidth() int {
	w, _, err := terminal.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 0
	}
	return w
}

// restoreTerminal installs a cleanup handler that restores the previous
// terminal state on exit.
func restoreTerminal() {
	if !stdoutIsTerminal() {
		return
	}

	fd := int(os.Stdout.Fd())
	state, err := terminal.GetState(fd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to get terminal state: %v\n", err)
		return
	}

	AddCleanupHandler(func() error {
		err := checkErrno(terminal.Restore(fd, state))
		if err != nil {
			fmt.Fprintf(os.Stderr, "unable to get restore terminal state: %#+v\n", err)
		}
		return err
	})
}

// ClearLine creates a platform dependent string to clear the current
// line, so it can be overwritten. ANSI sequences are not supported on
// current windows cmd shell.
func ClearLine() string {
	if runtime.GOOS == "windows" {
		if w := stdoutTerminalWidth(); w > 0 {
			return strings.Repeat(" ", w-1) + "\r"
		}
		return ""
	}
	return "\x1b[2K"
}

// Printf writes the message to the configured stdout stream.
func Printf(format string, args ...interface{}) {
	_, err := fmt.Fprintf(globalOptions.stdout, format, args...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to write to stdout: %v\n", err)
		Exit(100)
	}
}

// Verbosef calls Printf to write the message when the verbose flag is set.
func Verbosef(format string, args ...interface{}) {
	if globalOptions.Quiet {
		return
	}

	Printf(format, args...)
}

// PrintProgress wraps fmt.Printf to handle the difference in writing progress
// information to terminals and non-terminal stdout
func PrintProgress(format string, args ...interface{}) {
	var (
		message         string
		carriageControl string
	)
	message = fmt.Sprintf(format, args...)

	if !(strings.HasSuffix(message, "\r") || strings.HasSuffix(message, "\n")) {
		if stdoutIsTerminal() {
			carriageControl = "\r"
		} else {
			carriageControl = "\n"
		}
		message = fmt.Sprintf("%s%s", message, carriageControl)
	}

	if stdoutIsTerminal() {
		message = fmt.Sprintf("%s%s", ClearLine(), message)
	}

	fmt.Print(message)
}

// Warnf writes the message to the configured stderr stream.
func Warnf(format string, args ...interface{}) {
	_, err := fmt.Fprintf(globalOptions.stderr, format, args...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to write to stderr: %v\n", err)
		Exit(100)
	}
}

// Exitf uses Warnf to write the message and then terminates the process with
// the given exit code.
func Exitf(exitcode int, format string, args ...interface{}) {
	if format[len(format)-1] != '\n' {
		format += "\n"
	}

	Warnf(format, args...)
	Exit(exitcode)
}

// resolvePassword determines the password to be used for opening the repository.
func resolvePassword(opts GlobalOptions, env string) (string, error) {
	if opts.PasswordFile != "" {
		s, err := ioutil.ReadFile(opts.PasswordFile)
		if os.IsNotExist(err) {
			return "", errors.Fatalf("%s does not exist", opts.PasswordFile)
		}
		return strings.TrimSpace(string(s)), errors.Wrap(err, "Readfile")
	}

	if pwd := os.Getenv(env); pwd != "" {
		return pwd, nil
	}

	return "", nil
}

// readPassword reads the password from the given reader directly.
func readPassword(in io.Reader) (password string, err error) {
	buf := make([]byte, 1000)
	n, err := io.ReadFull(in, buf)
	buf = buf[:n]

	if err != nil && errors.Cause(err) != io.ErrUnexpectedEOF {
		return "", errors.Wrap(err, "ReadFull")
	}

	return strings.TrimRight(string(buf), "\r\n"), nil
}

// readPasswordTerminal reads the password from the given reader which must be a
// tty. Prompt is printed on the writer out before attempting to read the
// password.
func readPasswordTerminal(in *os.File, out io.Writer, prompt string) (password string, err error) {
	fmt.Fprint(out, prompt)
	buf, err := terminal.ReadPassword(int(in.Fd()))
	fmt.Fprintln(out)
	if err != nil {
		return "", errors.Wrap(err, "ReadPassword")
	}

	password = string(buf)
	return password, nil
}

// ReadPassword reads the password from a password file, the environment
// variable RESTIC_PASSWORD or prompts the user.
func ReadPassword(opts GlobalOptions, prompt string) (string, error) {
	if opts.password != "" {
		return opts.password, nil
	}

	var (
		password string
		err      error
	)

	if stdinIsTerminal() {
		password, err = readPasswordTerminal(os.Stdin, os.Stderr, prompt)
	} else {
		password, err = readPassword(os.Stdin)
	}

	if err != nil {
		return "", errors.Wrap(err, "unable to read password")
	}

	if len(password) == 0 {
		return "", errors.Fatal("an empty password is not a password")
	}

	return password, nil
}

// ReadPasswordTwice calls ReadPassword two times and returns an error when the
// passwords don't match.
func ReadPasswordTwice(gopts GlobalOptions, prompt1, prompt2 string) (string, error) {
	pw1, err := ReadPassword(gopts, prompt1)
	if err != nil {
		return "", err
	}
	pw2, err := ReadPassword(gopts, prompt2)
	if err != nil {
		return "", err
	}

	if pw1 != pw2 {
		return "", errors.Fatal("passwords do not match")
	}

	return pw1, nil
}

const maxKeys = 20

// OpenRepository reads the password and opens the repository.
func OpenRepository(opts GlobalOptions) (*repository.Repository, error) {
	if opts.Repo == "" {
		return nil, errors.Fatal("Please specify repository location (-r)")
	}

	be, err := open(opts.Repo, opts.extended)
	if err != nil {
		return nil, err
	}

	s := repository.New(be)

	opts.password, err = ReadPassword(opts, "enter password for repository: ")
	if err != nil {
		return nil, err
	}

	err = s.SearchKey(context.TODO(), opts.password, maxKeys)
	if err != nil {
		return nil, errors.Fatalf("unable to open repo: %v", err)
	}

	return s, nil
}

func parseConfig(loc location.Location, opts options.Options) (interface{}, error) {
	// only apply options for a particular backend here
	opts = opts.Extract(loc.Scheme)

	switch loc.Scheme {
	case "local":
		cfg := loc.Config.(local.Config)
		if err := opts.Apply(loc.Scheme, &cfg); err != nil {
			return nil, err
		}

		debug.Log("opening local repository at %#v", cfg)
		return cfg, nil

	case "sftp":
		cfg := loc.Config.(sftp.Config)
		if err := opts.Apply(loc.Scheme, &cfg); err != nil {
			return nil, err
		}

		debug.Log("opening sftp repository at %#v", cfg)
		return cfg, nil

	case "s3":
		cfg := loc.Config.(s3.Config)
		if cfg.KeyID == "" {
			cfg.KeyID = os.Getenv("AWS_ACCESS_KEY_ID")
		}

		if cfg.Secret == "" {
			cfg.Secret = os.Getenv("AWS_SECRET_ACCESS_KEY")
		}

		if err := opts.Apply(loc.Scheme, &cfg); err != nil {
			return nil, err
		}

		debug.Log("opening s3 repository at %#v", cfg)
		return cfg, nil

	case "gs":
		cfg := loc.Config.(gs.Config)
		if cfg.ProjectID == "" {
			cfg.ProjectID = os.Getenv("GOOGLE_PROJECT_ID")
		}

		if cfg.JSONKeyPath == "" {
			if path := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); path != "" {
				// Check read access
				if _, err := ioutil.ReadFile(path); err != nil {
					return nil, errors.Fatalf("Failed to read google credential from file %v: %v", path, err)
				}
				cfg.JSONKeyPath = path
			} else {
				return nil, errors.Fatal("No credential file path is set")
			}
		}

		if err := opts.Apply(loc.Scheme, &cfg); err != nil {
			return nil, err
		}

		debug.Log("opening gs repository at %#v", cfg)
		return cfg, nil

	case "azure":
		cfg := loc.Config.(azure.Config)
		if cfg.AccountName == "" {
			cfg.AccountName = os.Getenv("AZURE_ACCOUNT_NAME")
		}

		if cfg.AccountKey == "" {
			cfg.AccountKey = os.Getenv("AZURE_ACCOUNT_KEY")
		}

		if err := opts.Apply(loc.Scheme, &cfg); err != nil {
			return nil, err
		}

		debug.Log("opening gs repository at %#v", cfg)
		return cfg, nil

	case "swift":
		cfg := loc.Config.(swift.Config)

		if err := swift.ApplyEnvironment("", &cfg); err != nil {
			return nil, err
		}

		if err := opts.Apply(loc.Scheme, &cfg); err != nil {
			return nil, err
		}

		debug.Log("opening swift repository at %#v", cfg)
		return cfg, nil

	case "b2":
		cfg := loc.Config.(b2.Config)

		if cfg.AccountID == "" {
			cfg.AccountID = os.Getenv("B2_ACCOUNT_ID")
		}

		if cfg.Key == "" {
			cfg.Key = os.Getenv("B2_ACCOUNT_KEY")
		}

		if err := opts.Apply(loc.Scheme, &cfg); err != nil {
			return nil, err
		}

		debug.Log("opening b2 repository at %#v", cfg)
		return cfg, nil
	case "rest":
		cfg := loc.Config.(rest.Config)
		if err := opts.Apply(loc.Scheme, &cfg); err != nil {
			return nil, err
		}

		debug.Log("opening rest repository at %#v", cfg)
		return cfg, nil
	}

	return nil, errors.Fatalf("invalid backend: %q", loc.Scheme)
}

// Open the backend specified by a location config.
func open(s string, opts options.Options) (restic.Backend, error) {
	debug.Log("parsing location %v", s)
	loc, err := location.Parse(s)
	if err != nil {
		return nil, errors.Fatalf("parsing repository location failed: %v", err)
	}

	var be restic.Backend

	cfg, err := parseConfig(loc, opts)
	if err != nil {
		return nil, err
	}

	switch loc.Scheme {
	case "local":
		be, err = local.Open(cfg.(local.Config))
	case "sftp":
		be, err = sftp.Open(cfg.(sftp.Config))
	case "s3":
		be, err = s3.Open(cfg.(s3.Config))
	case "gs":
		be, err = gs.Open(cfg.(gs.Config))
	case "azure":
		be, err = azure.Open(cfg.(azure.Config))
	case "swift":
		be, err = swift.Open(cfg.(swift.Config))
	case "b2":
		be, err = b2.Open(cfg.(b2.Config))
	case "rest":
		be, err = rest.Open(cfg.(rest.Config))

	default:
		return nil, errors.Fatalf("invalid backend: %q", loc.Scheme)
	}

	if err != nil {
		return nil, errors.Fatalf("unable to open repo at %v: %v", s, err)
	}

	// check if config is there
	fi, err := be.Stat(context.TODO(), restic.Handle{Type: restic.ConfigFile})
	if err != nil {
		return nil, errors.Fatalf("unable to open config file: %v\nIs there a repository at the following location?\n%v", err, s)
	}

	if fi.Size == 0 {
		return nil, errors.New("config file has zero size, invalid repository?")
	}

	return be, nil
}

// Create the backend specified by URI.
func create(s string, opts options.Options) (restic.Backend, error) {
	debug.Log("parsing location %v", s)
	loc, err := location.Parse(s)
	if err != nil {
		return nil, err
	}

	cfg, err := parseConfig(loc, opts)
	if err != nil {
		return nil, err
	}

	switch loc.Scheme {
	case "local":
		return local.Create(cfg.(local.Config))
	case "sftp":
		return sftp.Create(cfg.(sftp.Config))
	case "s3":
		return s3.Create(cfg.(s3.Config))
	case "gs":
		return gs.Create(cfg.(gs.Config))
	case "azure":
		return azure.Create(cfg.(azure.Config))
	case "swift":
		return swift.Open(cfg.(swift.Config))
	case "b2":
		return b2.Create(cfg.(b2.Config))
	case "rest":
		return rest.Create(cfg.(rest.Config))
	}

	debug.Log("invalid repository scheme: %v", s)
	return nil, errors.Fatalf("invalid scheme %q", loc.Scheme)
}
