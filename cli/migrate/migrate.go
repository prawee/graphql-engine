// Package migrate implements migrations on Hasura GraphQL Engine.
//
// This package is borrowed from https://github.com/golang-migrate/migrate with
// additions for Hasura specific yaml file support and a improved Rails-like
// migration pattern.
package migrate

import (
	"bytes"
	"container/list"
	"crypto/tls"
	"fmt"
	"io"
	"os"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/cheggaaa/pb/v3"
	"golang.org/x/term"

	"github.com/hasura/graphql-engine/cli/v2/internal/hasura"

	"github.com/hasura/graphql-engine/cli/v2/util"

	"github.com/hasura/graphql-engine/cli/v2/migrate/database"
	"github.com/hasura/graphql-engine/cli/v2/migrate/source"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

// DefaultPrefetchMigrations sets the number of migrations to pre-read
// from the source. This is helpful if the source is remote, but has little
// effect for a local source (i.e. file system).
// Please note that this setting has a major impact on the memory usage,
// since each pre-read migration is buffered in memory. See DefaultBufferSize.
var DefaultPrefetchMigrations = uint64(10)

// DefaultLockTimeout sets the max time a database driver has to acquire a lock.
var DefaultLockTimeout = 15 * time.Second

var (
	ErrNoChange         = fmt.Errorf("no change")
	ErrNilVersion       = fmt.Errorf("no migration")
	ErrLocked           = fmt.Errorf("database locked")
	ErrNoMigrationFiles = fmt.Errorf("no migration files found")
	ErrLockTimeout      = fmt.Errorf("timeout: can't acquire database lock")
	ErrApplied          = fmt.Errorf("Version already applied in database")
	ErrNotApplied       = fmt.Errorf("Migration not applied in database")
	ErrNoMigrationMode  = fmt.Errorf("Migration mode is disabled")
	ErrMigrationMode    = fmt.Errorf("Migration mode is enabled")
)

const (
	applyingMigrationsMessage = "Applying migrations"
)

func newProgressBar(str string, w io.Writer, pbLogs bool) *pb.ProgressBar {
	// Default behaviour in non-interactive mode
	if !pbLogs && !term.IsTerminal(int(os.Stdout.Fd())) {
		return nil
	}
	// bar template configuration
	str = fmt.Sprintf(`"%v: "`, str)
	var barTemplateConfiguration string = fmt.Sprintf(`{{ cyan %s }} {{ counters .}} {{ bar . "[" "=" ">" "." "]"}} {{percent .}}`, str)
	bar := pb.ProgressBarTemplate(barTemplateConfiguration).New(0)
	bar.SetRefreshRate(time.Millisecond)
	// non-interactive mode with progressbar-logs flag
	if pbLogs && !term.IsTerminal(int(os.Stdout.Fd())) {
		bar.Set(pb.Terminal, true)
	}
	bar.Set(pb.CleanOnFinish, true)
	if w != nil {
		bar.SetWriter(w)
	}
	return bar
}

func startProgressBar(bar *pb.ProgressBar) {
	if bar != nil {
		bar.Start()
	}
}

func finishProgressBar(bar *pb.ProgressBar) {
	if bar != nil {
		bar.Finish()
	}
}

func incrementProgressBar(bar *pb.ProgressBar) {
	if bar != nil {
		bar.Increment()
	}
}

func setTotalProgressBar(bar *pb.ProgressBar, val int64) {
	if bar != nil {
		bar.SetTotal(val)
	}
}

func setWidthProgressBar(bar *pb.ProgressBar, pbLogs bool) {
	if pbLogs && !term.IsTerminal(int(os.Stdout.Fd())) && bar.Width() == 0 {
		bar.SetWidth(220)
	}
}

// ErrShortLimit is an error returned when not enough migrations
// can be returned by a source for a given limit.
type ErrShortLimit struct {
	Short uint64
}

// Error implements the error interface.
func (e ErrShortLimit) Error() string {
	return fmt.Sprintf("limit %v short", e.Short)
}

type ErrDirty struct {
	Version int64
}

func (e ErrDirty) Error() string {
	return fmt.Sprintf("Dirty database version %v. Fix and force version.", e.Version)
}

type Migrate struct {
	sourceName string
	sourceURL  string
	sourceDrv  source.Driver

	databaseName string
	databaseURL  string
	databaseDrv  database.Driver

	// Logger is the global logger object to print logs.
	Logger *log.Logger
	stderr io.Writer
	stdout io.Writer

	// GracefulStop accepts `true` and will stop executing migrations
	// as soon as possible at a safe break point, so that the database
	// is not corrupted.
	GracefulStop   chan bool
	isGracefulStop bool

	isLockedMu *sync.Mutex
	isLocked   bool

	// PrefetchMigrations defaults to DefaultPrefetchMigrations,
	// but can be set per Migrate instance.
	PrefetchMigrations uint64

	// LockTimeout defaults to DefaultLockTimeout,
	// but can be set per Migrate instance.
	LockTimeout time.Duration

	//CMD
	isCMD bool

	status *Status

	SkipExecution   bool
	DryRun          bool
	ProgressBarLogs bool
}

type NewMigrateOpts struct {
	sourceUrl, databaseUrl string
	cmd                    bool
	configVersion          int
	tlsConfig              *tls.Config
	logger                 *log.Logger
	Stdout                 io.Writer
	Stderr                 io.Writer
	hasuraOpts             *database.HasuraOpts
}

// New returns a new Migrate instance from a source URL and a database URL.
// The URL scheme is defined by each driver.
func New(opts NewMigrateOpts) (*Migrate, error) {
	m := newCommon(opts.cmd)
	m.stdout = opts.Stdout
	m.stderr = opts.Stderr

	sourceName, err := schemeFromUrl(opts.sourceUrl)
	if err != nil {
		log.Debug(err)
		return nil, err
	}
	m.sourceName = sourceName
	m.sourceURL = opts.sourceUrl

	databaseName, err := schemeFromUrl(opts.databaseUrl)
	if err != nil {
		log.Debug(err)
		return nil, err
	}
	m.databaseName = databaseName
	m.databaseURL = opts.databaseUrl

	if opts.logger == nil {
		opts.logger = log.New()
	}
	m.Logger = opts.logger

	sourceDrv, err := source.Open(opts.sourceUrl, opts.logger)
	if err != nil {
		log.Debug(err)
		return nil, err
	}
	m.sourceDrv = sourceDrv
	if opts.configVersion >= 2 {
		m.sourceDrv.DefaultParser(source.DefaultParsev2)
	}

	databaseDrv, err := database.Open(opts.databaseUrl, opts.cmd, opts.tlsConfig, opts.logger, opts.hasuraOpts)
	if err != nil {
		log.Debug(err)
		return nil, err
	}
	m.databaseDrv = databaseDrv

	err = m.ReScan()
	if err != nil {
		return nil, err
	}
	return m, nil
}

func newCommon(cmd bool) *Migrate {
	return &Migrate{
		GracefulStop:       make(chan bool, 1),
		PrefetchMigrations: DefaultPrefetchMigrations,
		isLockedMu:         &sync.Mutex{},
		LockTimeout:        DefaultLockTimeout,
		isCMD:              cmd,
	}
}

func (m *Migrate) ReScan() error {
	err := m.sourceDrv.Scan()
	if err != nil {
		m.Logger.Debug(err)
		return err
	}

	err = m.databaseDrv.Scan()
	if err != nil {
		m.Logger.Debug(err)
		return err
	}

	err = m.calculateStatus()
	if err != nil {
		return err
	}
	return nil
}

// Close closes the source and the database.
func (m *Migrate) Close() (source error) {
	sourceSrvClose := make(chan error)

	go func() {
		sourceSrvClose <- m.sourceDrv.Close()
	}()

	return <-sourceSrvClose
}

func (m *Migrate) calculateStatus() (err error) {
	m.status = NewStatus()
	err = m.readStatusFromSource()
	if err != nil {
		return err
	}

	return m.readStatusFromDatabase()
}

func (m *Migrate) readStatusFromSource() (err error) {
	firstVersion, err := m.sourceDrv.First()
	if err != nil {
		if _, ok := err.(*os.PathError); ok {
			return nil
		}
		return err
	}
	m.status.Append(m.newMigrationStatus(firstVersion, "source", false))
	from := int64(firstVersion)

	lastVersion, err := m.sourceDrv.GetLocalVersion()
	if err != nil {
		return err
	}
	m.status.Append(m.newMigrationStatus(lastVersion, "source", false))
	to := int64(lastVersion)

	for from < to {
		next, err := m.sourceDrv.Next(suint64(from))
		if err != nil {
			return err
		}
		m.status.Append(m.newMigrationStatus(next, "source", false))
		from = int64(next)
	}

	return nil
}

func (m *Migrate) readStatusFromDatabase() (err error) {
	firstVersion, ok := m.databaseDrv.First()
	if !ok {
		return nil
	}
	m.status.Append(m.newMigrationStatus(firstVersion.Version, "database", firstVersion.Dirty))
	from := int64(firstVersion.Version)

	lastVersion, ok := m.databaseDrv.Last()
	if !ok {
		return nil
	}
	m.status.Append(m.newMigrationStatus(lastVersion.Version, "database", lastVersion.Dirty))
	to := int64(lastVersion.Version)

	for from < to {
		next, ok := m.databaseDrv.Next(suint64(from))
		if !ok {
			return nil
		}
		m.status.Append(m.newMigrationStatus(next.Version, "database", next.Dirty))
		from = int64(next.Version)
	}
	return err
}

func (m *Migrate) newMigrationStatus(version uint64, driverType string, dirty bool) *MigrationStatus {
	var migrStatus *MigrationStatus
	migrStatus, ok := m.status.Read(version)
	if !ok {
		migrStatus = &MigrationStatus{
			Version: version,
			Name:    m.sourceDrv.ReadName(version),
			IsDirty: dirty,
		}
	}

	switch driverType {
	case "source":
		migrStatus.IsPresent = true
	case "database":
		migrStatus.IsApplied = true
		migrStatus.IsDirty = dirty
	default:
		return nil
	}
	return migrStatus
}

func (m *Migrate) GetStatus() (*Status, error) {
	err := m.calculateStatus()
	if err != nil {
		return nil, err
	}
	return m.status, nil
}

func (m *Migrate) GetSetting(name string) (string, error) {
	val, err := m.databaseDrv.GetSetting(name)
	if err != nil {
		return "", err
	}
	return val, nil
}

func (m *Migrate) UpdateSetting(name string, value string) error {
	return m.databaseDrv.UpdateSetting(name, value)
}

func (m *Migrate) Version() (version uint64, dirty bool, err error) {
	v, d, err := m.databaseDrv.Version()
	if err != nil {
		return 0, false, err
	}

	if v == database.NilVersion {
		return 0, false, ErrNilVersion
	}

	return suint64(v), d, nil
}

func (m *Migrate) GetUnappliedMigrations(version uint64) []uint64 {
	return m.sourceDrv.GetUnappliedMigrations(version)
}

func (m *Migrate) ExportSchemaDump(schemName []string, sourceName string, sourceKind hasura.SourceKind) ([]byte, error) {
	return m.databaseDrv.ExportSchemaDump(schemName, sourceName, sourceKind)
}

func (m *Migrate) RemoveVersions(versions []uint64) error {
	mode, err := m.databaseDrv.GetSetting("migration_mode")
	if err != nil {
		return err
	}

	if mode != "true" {
		return ErrNoMigrationMode
	}

	if err := m.lock(); err != nil {
		return err
	}

	for _, version := range versions {
		m.databaseDrv.RemoveVersion(int64(version))
	}
	return m.unlockErr(nil)
}

func (m *Migrate) Query(data interface{}) error {
	mode, err := m.databaseDrv.GetSetting("migration_mode")
	if err != nil {
		return err
	}

	if mode == "true" {
		return ErrMigrationMode
	}
	return m.databaseDrv.Query(data)
}

// Squash migrations from version v into a new migration.
// Returns a list of migrations that are squashed: vs
// the squashed metadata for all UP steps: um
// the squashed SQL for all UP steps: us
// the squashed metadata for all down steps: dm
// the squashed SQL for all down steps: ds
func (m *Migrate) Squash(v uint64) (vs []int64, us []byte, ds []byte, err error) {
	// check the migration mode on the database
	mode, err := m.databaseDrv.GetSetting("migration_mode")
	if err != nil {
		return
	}

	// if migration_mode is false, set err to ErrNoMigrationMode and return
	if mode != "true" {
		err = ErrNoMigrationMode
		return
	}

	// concurrently squash all the up migrations
	// read all up migrations from source and send each migration
	// to the returned channel
	retUp := make(chan interface{}, m.PrefetchMigrations)
	go m.squashUp(v, retUp)

	// concurrently squash all down migrations
	// read all down migrations from source and send each migration
	// to the returned channel
	retDown := make(chan interface{}, m.PrefetchMigrations)
	go m.squashDown(v, retDown)

	// combine squashed up and down migrations into a single one when they're ready
	dataUp := make(chan interface{}, m.PrefetchMigrations)
	dataDown := make(chan interface{}, m.PrefetchMigrations)
	retVersions := make(chan int64, m.PrefetchMigrations)
	go m.squashMigrations(retUp, retDown, dataUp, dataDown, retVersions)

	// make a chan for errors
	errChn := make(chan error, 2)

	// create a waitgroup to wait for all goroutines to finish execution
	var wg sync.WaitGroup
	// add three tasks to waitgroup since we used 3 goroutines above
	wg.Add(3)

	// read from dataUp chan when all up migrations are squashed and compiled
	go func() {
		// defer to mark one task in the waitgroup as complete
		defer wg.Done()

		buf := &bytes.Buffer{}
		for r := range dataUp {
			// check the type of value returned through the chan
			switch data := r.(type) {
			case error:
				// it's an error, set error and return
				// note: this return is returning the goroutine, not the current function
				m.isGracefulStop = true
				errChn <- r.(error)
				return
			case []byte:
				// it's SQL, concat all of them
				buf.WriteString("\n")
				buf.Write(data)
			}
		}
		// set us as the bytes written into buf
		us = buf.Bytes()
	}()

	// read from dataDown when it is ready:
	go func() {
		// defer to mark another task in the waitgroup as complete
		defer wg.Done()
		buf := &bytes.Buffer{}
		for r := range dataDown {
			// check the type of value returned through the chan
			switch data := r.(type) {
			case error:
				// it's an error, set error and return
				// note: this return is returning the goroutine, not the current function
				m.isGracefulStop = true
				errChn <- r.(error)
				return
			case []byte:
				// it's SQL, concat all of them
				buf.WriteString("\n")
				buf.Write(data)
			}
		}
		// set ds as the bytes written into buf
		ds = buf.Bytes()
	}()

	// read retVersions - versions that are squashed
	go func() {
		// defer to mark another task in the waitgroup as complete
		defer wg.Done()
		for r := range retVersions {
			// append each version into the versions array
			vs = append(vs, r)
		}
	}()

	// returns from the above goroutines pass the control here.

	// wait until all tasks (3) in the workgroup are completed
	wg.Wait()

	// close the errChn
	close(errChn)

	// check for errors in the error channel
	for e := range errChn {
		err = e
		return
	}

	return
}

// Migrate looks at the currently active migration version,
// then migrates either up or down to the specified version.
func (m *Migrate) Migrate(version uint64, direction string) error {
	mode, err := m.databaseDrv.GetSetting("migration_mode")
	if err != nil {
		return err
	}

	if mode != "true" {
		return ErrNoMigrationMode
	}

	if err := m.lock(); err != nil {
		return err
	}

	ret := make(chan interface{}, m.PrefetchMigrations)
	bar := newProgressBar(applyingMigrationsMessage, m.stderr, m.ProgressBarLogs)
	go m.read(version, direction, ret, bar)
	if m.DryRun {
		return m.unlockErr(m.runDryRun(ret))
	} else {
		return m.unlockErr(m.runMigrations(ret, bar))
	}
}

func (m *Migrate) QueryWithVersion(version uint64, data io.ReadCloser, skipExecution bool) error {
	mode, err := m.databaseDrv.GetSetting("migration_mode")
	if err != nil {
		return err
	}

	if mode != "true" {
		return ErrNoMigrationMode
	}

	if err := m.lock(); err != nil {
		return err
	}

	if !skipExecution {
		if err := m.databaseDrv.Run(data, "meta", ""); err != nil {
			m.databaseDrv.ResetQuery()
			return m.unlockErr(err)
		}
	}

	if version != 0 {
		if err := m.databaseDrv.SetVersion(int64(version), false); err != nil {
			m.databaseDrv.ResetQuery()
			return m.unlockErr(err)
		}
	}
	return m.unlockErr(nil)
}

// Steps looks at the currently active migration version.
// It will migrate up if n > 0, and down if n < 0.
func (m *Migrate) Steps(n int64) error {
	mode, err := m.databaseDrv.GetSetting("migration_mode")
	if err != nil {
		return err
	}

	if mode != "true" {
		return ErrNoMigrationMode
	}

	if n == 0 {
		return ErrNoChange
	}

	if err := m.lock(); err != nil {
		return err
	}

	ret := make(chan interface{}, m.PrefetchMigrations)
	bar := newProgressBar(applyingMigrationsMessage, m.stderr, m.ProgressBarLogs)

	if n > 0 {
		go m.readUp(n, ret, bar)
	} else {
		go m.readDown(-n, ret, bar)
	}

	if m.DryRun {
		return m.unlockErr(m.runDryRun(ret))
	} else {
		return m.unlockErr(m.runMigrations(ret, bar))
	}
}

// Up looks at the currently active migration version
// and will migrate all the way up (applying all up migrations).
func (m *Migrate) Up() error {
	mode, err := m.databaseDrv.GetSetting("migration_mode")
	if err != nil {
		return err
	}

	if mode != "true" {
		return ErrNoMigrationMode
	}

	if err := m.lock(); err != nil {
		return err
	}

	curVersion, dirty, err := m.databaseDrv.Version()
	if err != nil {
		return err
	}

	if dirty {
		return ErrDirty{curVersion}
	}

	ret := make(chan interface{}, m.PrefetchMigrations)
	bar := newProgressBar(applyingMigrationsMessage, m.stderr, m.ProgressBarLogs)
	go m.readUp(-1, ret, bar)

	if m.DryRun {
		return m.unlockErr(m.runDryRun(ret))
	} else {
		return m.unlockErr(m.runMigrations(ret, bar))
	}
}

// Down looks at the currently active migration version
// and will migrate all the way down (applying all down migrations).
func (m *Migrate) Down() error {
	mode, err := m.databaseDrv.GetSetting("migration_mode")
	if err != nil {
		return err
	}

	if mode != "true" {
		return ErrNoMigrationMode
	}

	if err := m.lock(); err != nil {
		return err
	}

	curVersion, dirty, err := m.databaseDrv.Version()
	if err != nil {
		return err
	}

	if dirty {
		return ErrDirty{curVersion}
	}

	ret := make(chan interface{}, m.PrefetchMigrations)
	bar := newProgressBar(applyingMigrationsMessage, m.stderr, m.ProgressBarLogs)
	go m.readDown(-1, ret, bar)

	if m.DryRun {
		return m.unlockErr(m.runDryRun(ret))
	} else {
		return m.unlockErr(m.runMigrations(ret, bar))
	}
}

func (m *Migrate) squashUp(version uint64, ret chan<- interface{}) {
	defer close(ret)
	currentVersion := version
	count := int64(0)
	limit := int64(-1)
	if m.stop() {
		return
	}

	for limit == -1 {
		if currentVersion == version {
			// during the first iteration of the loop
			// check if a next version exists for "--from" version
			if err := m.versionUpExists(version); err != nil {
				ret <- err
				return
			}

			// If next version exists this function will return an instance of
			// migration.go.Migrate struct
			// this reads the SQL up migration
			// even if a migration file does'nt exist in the source
			// a empty migration will be returned
			migr, err := m.newMigration(version, int64(version))
			if err != nil {
				ret <- err
				return
			}
			ret <- migr
			// write the body of the migration to reader
			// the migr instance sent via the channel will then start reading
			// from it
			go migr.Buffer()

			count++
		}

		// get the next version using source driver
		// earlier in the first iteration we knew what version to operate on
		// but here we have to find the next version
		next, err := m.sourceDrv.Next(currentVersion)
		if os.IsNotExist(err) {
			// no limit, but no migrations applied?
			if count == 0 {
				ret <- ErrNoChange
				return
			}
			// when there is no more migrations return
			if limit == -1 {
				return
			}
		}

		if err != nil {
			ret <- err
			return
		}

		// Check if next files exists (yaml or sql)
		if err = m.versionUpExists(next); err != nil {
			ret <- err
			return
		}

		migr, err := m.newMigration(next, int64(next))
		if err != nil {
			ret <- err
			return
		}

		ret <- migr
		go migr.Buffer()

		currentVersion = next
		count++
	}
}

func (m *Migrate) squashDown(version uint64, ret chan<- interface{}) {
	defer close(ret)

	// get the last version from the source driver
	from, err := m.sourceDrv.GetLocalVersion()
	if err != nil {
		ret <- err
		return
	}

	for {
		if m.stop() {
			return
		}

		if from < version {
			return
		}

		prev, err := m.sourceDrv.Prev(from)
		if os.IsNotExist(err) {
			migr, err := m.newMigration(from, -1)
			if err != nil {
				ret <- err
				return
			}
			ret <- migr
			go migr.Buffer()
			return
		} else if err != nil {
			ret <- err
			return
		}

		err = m.versionDownExists(from)
		if err == nil {
			migr, err := m.newMigration(from, int64(prev))
			if err != nil {
				ret <- err
				return
			}

			ret <- migr
			go migr.Buffer()
		} else {
			m.Logger.Warnf("%v", err)
		}

		from = prev
	}
}

// read reads either up or down migrations from source `from` to `to`.
// Each migration is then written to the ret channel.
// If an error occurs during reading, that error is written to the ret channel, too.
// Once read is done reading it will close the ret channel.
func (m *Migrate) read(version uint64, direction string, ret chan<- interface{}, bar *pb.ProgressBar) {
	defer close(ret)

	setTotalProgressBar(bar, 1)

	if direction == "up" {
		if m.stop() {
			return
		}

		// Check if this version present in DB
		ok := m.databaseDrv.Read(version)
		if ok {
			ret <- ErrApplied
			return
		}

		// Check if next version exists (yaml or sql)
		if err := m.versionUpExists(version); err != nil {
			ret <- err
			return
		}

		migr, err := m.newMigration(version, int64(version))
		if err != nil {
			ret <- err
			return
		}

		ret <- migr
		go migr.Buffer()
	} else {
		// it's going down
		if m.stop() {
			return
		}

		// Check if this version present in DB
		ok := m.databaseDrv.Read(version)
		if !ok {
			ret <- ErrNotApplied
			return
		}

		if err := m.versionDownExists(version); err != nil {
			ret <- err
			return
		}

		prev, err := m.sourceDrv.Prev(version)
		if os.IsNotExist(err) {
			migr, err := m.newMigration(version, -1)
			if err != nil {
				ret <- err
				return
			}
			ret <- migr
			go migr.Buffer()
			return
		} else if err != nil {
			ret <- err
			return
		}

		migr, err := m.newMigration(version, int64(prev))
		if err != nil {
			ret <- err
			return
		}

		ret <- migr
		go migr.Buffer()
	}
}

// readUp reads up migrations from `from` limitted by `limit`.
// limit can be -1, implying no limit and reading until there are no more migrations.
// Each migration is then written to the ret channel.
// If an error occurs during reading, that error is written to the ret channel, too.
// Once readUp is done reading it will close the ret channel.
func (m *Migrate) readUp(limit int64, ret chan<- interface{}, bar *pb.ProgressBar) {
	defer close(ret)

	if limit == 0 {
		ret <- ErrNoChange
		return
	}

	if limit == -1 { // To get no of migrations to be applied on  server if limit is -1
		noOfUnappliedMigrations := 0
		for _, migration := range m.status.Migrations {
			if migration.IsPresent && !migration.IsApplied { // needs to be applied
				noOfUnappliedMigrations++
			}
		}

		if noOfUnappliedMigrations == 0 {
			ret <- ErrNoChange
			return
		}

		setTotalProgressBar(bar, int64(noOfUnappliedMigrations))
	} else {
		setTotalProgressBar(bar, limit)
	}

	count := int64(0)
	from := int64(-1)
	for count < limit || limit == -1 {
		if m.stop() {
			return
		}

		if from == -1 {
			firstVersion, err := m.sourceDrv.First()
			if err != nil {
				ret <- err
				return
			}

			// Check if this version present in DB
			ok := m.databaseDrv.Read(firstVersion)
			if ok {
				from = int64(firstVersion)
				continue
			}

			// Check if firstVersion files exists (yaml or sql)
			if err = m.versionUpExists(firstVersion); err != nil {
				ret <- err
				return
			}

			migr, err := m.newMigration(firstVersion, int64(firstVersion))
			if err != nil {
				ret <- err
				return
			}
			ret <- migr
			go migr.Buffer()

			from = int64(firstVersion)
			count++
			continue
		}

		// apply next migration
		next, err := m.sourceDrv.Next(suint64(from))
		if os.IsNotExist(err) {
			// no limit, but no migrations applied?
			if limit == -1 && count == 0 {
				ret <- ErrNoChange
				return
			}

			// no limit, reached end
			if limit == -1 {
				return
			}

			// reached end, and didn't apply any migrations
			if limit > 0 && count == 0 {
				ret <- ErrNoChange
				return
			}

			// applied less migrations than limit?
			if count < limit {
				ret <- ErrShortLimit{suint64(limit - count)}
				return
			}
		}

		if err != nil {
			ret <- err
			return
		}

		// Check if this version present in DB
		ok := m.databaseDrv.Read(next)
		if ok {
			from = int64(next)
			continue
		}

		// Check if next files exists (yaml or sql)
		if err = m.versionUpExists(next); err != nil {
			ret <- err
			return
		}

		migr, err := m.newMigration(next, int64(next))
		if err != nil {
			ret <- err
			return
		}

		ret <- migr
		go migr.Buffer()

		from = int64(next)
		count++
	}
}

// readDown reads down migrations from `from` limitted by `limit`.
// limit can be -1, implying no limit and reading until there are no more migrations.
// Each migration is then written to the ret channel.
// If an error occurs during reading, that error is written to the ret channel, too.
// Once readDown is done reading it will close the ret channel.
func (m *Migrate) readDown(limit int64, ret chan<- interface{}, bar *pb.ProgressBar) {
	defer close(ret)

	if limit == 0 {
		ret <- ErrNoChange
		return
	}

	from, _, err := m.databaseDrv.Version()
	if err != nil {
		ret <- err
		return
	}

	// no change if already at nil version
	if from == -1 && limit == -1 {
		ret <- ErrNoChange
		return
	}

	// can't go over limit if already at nil version
	if from == -1 && limit > 0 {
		ret <- ErrNoChange
		return
	}

	if limit == -1 { // To get no of migrations to be rollback on  server if limit is -1
		noOfAppliedMigrations := 0
		for _, migration := range m.status.Migrations {
			if migration.IsPresent && migration.IsApplied { // needs to be applied
				noOfAppliedMigrations++
			}
		}
		setTotalProgressBar(bar, int64(noOfAppliedMigrations))
	} else {
		setTotalProgressBar(bar, limit)
	}

	count := int64(0)
	for count < limit || limit == -1 {
		if m.stop() {
			return
		}

		err = m.versionDownExists(suint64(from))
		if err != nil {
			ret <- err
			return
		}

		prev, ok := m.databaseDrv.Prev(suint64(from))
		if !ok {
			// no limit or haven't reached limit, apply "first" migration
			if limit == -1 || limit-count > 0 {
				migr, err := m.newMigration(suint64(from), -1)
				if err != nil {
					ret <- err
					return
				}
				ret <- migr
				go migr.Buffer()
				count++
			}

			if count < limit {
				ret <- ErrShortLimit{suint64(limit - count)}
			}
			return
		}

		migr, err := m.newMigration(suint64(from), int64(prev.Version))
		if err != nil {
			ret <- err
			return
		}

		ret <- migr
		go migr.Buffer()
		from = int64(prev.Version)
		count++
	}
}

// runMigrations reads *Migration and error from a channel. Any other type
// sent on this channel will result in a panic. Each migration is then
// proxied to the database driver and run against the database.
// Before running a newly received migration it will check if it's supposed
// to stop execution because it might have received a stop signal on the
// GracefulStop channel.
func (m *Migrate) runMigrations(ret <-chan interface{}, bar *pb.ProgressBar) error {
	startProgressBar(bar)
	setWidthProgressBar(bar, m.ProgressBarLogs)
	defer finishProgressBar(bar)
	for r := range ret {
		if m.stop() {
			return nil
		}

		switch r.(type) {
		case error:
			// Clear Migration query
			m.databaseDrv.ResetQuery()
			return r.(error)
		case *Migration:
			migr := r.(*Migration)
			if migr.Body != nil {
				if !m.SkipExecution {
					m.Logger.Debugf("applying migration: %s", migr.FileName)
					if err := m.databaseDrv.Run(migr.BufferedBody, migr.FileType, migr.FileName); err != nil {
						return err
					}
				}
				incrementProgressBar(bar)
				version := int64(migr.Version)
				// Insert Version number into the table
				if err := m.databaseDrv.SetVersion(version, false); err != nil {
					return err
				}
				if version != migr.TargetVersion {
					// Delete Version number from the table
					if err := m.databaseDrv.RemoveVersion(version); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func (m *Migrate) runDryRun(ret <-chan interface{}) error {
	migrations := make([]*Migration, 0)
	var lastInsertVersion int64
	for r := range ret {
		if m.stop() {
			return nil
		}

		switch r.(type) {
		case error:
			return r.(error)
		case *Migration:
			migr := r.(*Migration)
			if migr.Body != nil {
				version := int64(migr.Version)
				if version != lastInsertVersion {
					migrations = append(migrations, migr)
					lastInsertVersion = version
				}
			}
		}
	}
	fmt.Fprintf(os.Stdout, "%s", printDryRunStatus(migrations))
	return nil
}

func (m *Migrate) squashMigrations(retUp <-chan interface{}, retDown <-chan interface{}, dataUp chan<- interface{}, dataDown chan<- interface{}, versions chan<- int64) error {
	var latestVersion int64
	go func() {
		defer close(dataUp)
		defer close(versions)

		var err error

		squashList := database.CustomList{
			list.New(),
		}

		defer func() {
			if err == nil {
				m.databaseDrv.Squash(&squashList, dataUp)
			}
		}()

		for r := range retUp {
			if m.stop() {
				return
			}
			switch r.(type) {
			case error:
				dataUp <- r.(error)
			case *Migration:
				migr := r.(*Migration)
				if migr.Body != nil {
					// read migration body and push it to squash list
					if err = m.databaseDrv.PushToList(migr.BufferedBody, migr.FileType, &squashList); err != nil {
						dataUp <- err
						return
					}
				}

				version := int64(migr.Version)
				if version == migr.TargetVersion && version != latestVersion {
					versions <- version
					latestVersion = version
				}
			}
		}
	}()

	go func() {
		defer close(dataDown)
		var err error

		squashList := database.CustomList{
			list.New(),
		}

		defer func() {
			if err == nil {
				m.databaseDrv.Squash(&squashList, dataDown)
			}
		}()

		for r := range retDown {
			if m.stop() {
				return
			}
			switch r.(type) {
			case error:
				dataDown <- r.(error)
			case *Migration:
				migr := r.(*Migration)
				if migr.Body != nil {
					if err = m.databaseDrv.PushToList(migr.BufferedBody, migr.FileType, &squashList); err != nil {
						dataDown <- err
						return
					}
				}
			}
		}
	}()
	return nil
}

// versionUpExists checks the source if either the up or down migration for
// the specified migration version exists.
func (m *Migrate) versionUpExists(version uint64) error {
	// try up migration first
	directions := m.sourceDrv.GetDirections(version)
	if !directions[source.Up] && !directions[source.MetaUp] {
		return fmt.Errorf("%d up migration not found", version)
	}

	if directions[source.Up] {
		up, _, _, err := m.sourceDrv.ReadUp(version)
		if err == nil {
			defer up.Close()
		}

		if os.IsExist(err) {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
	}

	if directions[source.MetaUp] {
		up, _, _, err := m.sourceDrv.ReadMetaUp(version)
		if err == nil {
			defer up.Close()
		}

		if os.IsExist(err) {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
	}

	return os.ErrNotExist
}

// versionDownExists checks the source if either the up or down migration for
// the specified migration version exists.
func (m *Migrate) versionDownExists(version uint64) error {
	// try down migration first
	directions := m.sourceDrv.GetDirections(version)
	if !directions[source.Down] && !directions[source.MetaDown] {
		return fmt.Errorf("%d down migration not found", version)
	}

	if directions[source.Down] {
		up, _, _, err := m.sourceDrv.ReadDown(version)
		if err == nil {
			defer up.Close()
		}

		if os.IsExist(err) {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
	}

	if directions[source.MetaDown] {
		up, _, _, err := m.sourceDrv.ReadMetaDown(version)
		if err == nil {
			defer up.Close()
		}

		if os.IsExist(err) {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
	}

	return os.ErrNotExist
}

// newMigration is a helper func that returns a *Migration for the
// specified version and targetVersion (sql).
// will return the down migration
func (m *Migrate) newMigration(version uint64, targetVersion int64) (*Migration, error) {
	var migr *Migration

	if targetVersion >= int64(version) {
		r, identifier, fileName, err := m.sourceDrv.ReadUp(version)
		if os.IsNotExist(err) {
			// create "empty" migration
			migr, err = NewMigration(nil, "", version, targetVersion, "sql", "")
			if err != nil {
				return nil, err
			}

		} else if err != nil {
			return nil, err

		} else {
			// create migration from up source
			migr, err = NewMigration(r, identifier, version, targetVersion, "sql", fileName)
			if err != nil {
				return nil, err
			}
		}

	} else {
		r, identifier, fileName, err := m.sourceDrv.ReadDown(version)
		if os.IsNotExist(err) {
			// create "empty" migration
			migr, err = NewMigration(nil, "", version, targetVersion, "sql", "")
			if err != nil {
				return nil, err
			}

		} else if err != nil {
			return nil, err

		} else {
			// create migration from down source
			migr, err = NewMigration(r, identifier, version, targetVersion, "sql", fileName)
			if err != nil {
				return nil, err
			}
		}
	}

	if m.PrefetchMigrations > 0 && migr.Body != nil {
		//m.logVerbosePrintf("Start buffering %v\n", migr.LogString())
	} else {
		//m.logVerbosePrintf("Scheduled %v\n", migr.LogString())
	}

	return migr, nil
}

// stop returns true if no more migrations should be run against the database
// because a stop signal was received on the GracefulStop channel.
// Calls are cheap and this function is not blocking.
func (m *Migrate) stop() bool {
	if m.isGracefulStop {
		return true
	}

	select {
	case <-m.GracefulStop:
		m.isGracefulStop = true
		return true

	default:
		return false
	}
}

// lock is a thread safe helper function to lock the database.
// It should be called as late as possible when running migrations.
func (m *Migrate) lock() error {
	m.isLockedMu.Lock()
	defer m.isLockedMu.Unlock()

	if m.isLocked {
		return ErrLocked
	}

	// create done channel, used in the timeout goroutine
	done := make(chan bool, 1)
	defer func() {
		done <- true
	}()

	// use errchan to signal error back to this context
	errchan := make(chan error, 2)

	// start timeout goroutine
	timeout := time.After(m.LockTimeout)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-timeout:
				errchan <- ErrLockTimeout
				return
			}
		}
	}()

	// now try to acquire the lock
	go func() {
		if err := m.databaseDrv.Lock(); err != nil {
			errchan <- err
		} else {
			errchan <- nil
		}
		return
	}()

	// wait until we either recieve ErrLockTimeout or error from Lock operation
	err := <-errchan
	if err == nil {
		m.isLocked = true
	}
	return err
}

// unlock is a thread safe helper function to unlock the database.
// It should be called as early as possible when no more migrations are
// expected to be executed.
func (m *Migrate) unlock() error {
	m.isLockedMu.Lock()
	defer m.isLockedMu.Unlock()

	defer func() {
		m.isLocked = false
	}()
	return m.databaseDrv.UnLock()
}

// unlockErr calls unlock and returns a combined error
// if a prevErr is not nil.
func (m *Migrate) unlockErr(prevErr error) error {
	if err := m.unlock(); err != nil {
		return NewMultiError(prevErr, err)
	}
	return prevErr
}

// GotoVersion will apply a version also applying the migration chain
// leading to it
func (m *Migrate) GotoVersion(gotoVersion int64) error {
	mode, err := m.databaseDrv.GetSetting("migration_mode")
	if err != nil {
		return err
	}
	if mode != "true" {
		return ErrNoMigrationMode
	}

	currentVersion, dirty, err := m.Version()
	currVersion := int64(currentVersion)
	if err != nil {
		if err == ErrNilVersion {
			currVersion = database.NilVersion
		} else {
			return errors.Wrap(err, "cannot determine version")
		}
	}
	if dirty {
		return ErrDirty{currVersion}
	}

	if err := m.lock(); err != nil {
		return err
	}

	ret := make(chan interface{})
	bar := newProgressBar(applyingMigrationsMessage, m.stderr, m.ProgressBarLogs)
	if currVersion <= gotoVersion {
		go m.readUpFromVersion(-1, gotoVersion, ret, bar)
	} else if currVersion > gotoVersion {
		go m.readDownFromVersion(currVersion, gotoVersion, ret, bar)
	}

	if m.DryRun {
		return m.unlockErr(m.runDryRun(ret))
	} else {
		return m.unlockErr(m.runMigrations(ret, bar))
	}
}

// readUpFromVersion reads up migrations from `from` limitted by `limit`. (is a modified version of readUp)
// limit can be -1, implying no limit and reading until there are no more migrations.
// Each migration is then written to the ret channel.
// If an error occurs during reading, that error is written to the ret channel, too.
// Once readUpFromVersion is done reading it will close the ret channel.
func (m *Migrate) readUpFromVersion(from int64, to int64, ret chan<- interface{}, bar *pb.ProgressBar) {
	defer close(ret)
	var noOfUnappliedMigrations int = 0

	if to == -1 {
		ret <- ErrNoChange
		return
	}

	for _, i := range m.status.Migrations {
		if i.Version > uint64(to) {
			continue
		}
		if i.IsPresent && !i.IsApplied { // needs to be applied
			noOfUnappliedMigrations++
		}
	}

	if noOfUnappliedMigrations == 0 {
		ret <- ErrNoChange
		return
	}

	setTotalProgressBar(bar, int64(noOfUnappliedMigrations))

	for {
		if m.stop() {
			return
		}

		if from == to {
			return
		}

		if from == -1 {
			firstVersion, err := m.sourceDrv.First()
			if err != nil {
				ret <- err
				return
			}

			// Check if this version present in DB
			ok := m.databaseDrv.Read(firstVersion)
			if ok {
				from = int64(firstVersion)
				continue
			}

			// Check if firstVersion files exists (yaml or sql)
			if err = m.versionUpExists(firstVersion); err != nil {
				ret <- err
				return
			}

			migr, err := m.newMigration(firstVersion, int64(firstVersion))
			if err != nil {
				ret <- err
				return
			}
			ret <- migr
			go migr.Buffer()

			from = int64(firstVersion)
			continue
		}

		// apply next migration
		next, err := m.sourceDrv.Next(suint64(from))
		if err != nil {
			ret <- err
			return
		}

		// Check if this version present in DB
		ok := m.databaseDrv.Read(next)
		if ok {
			from = int64(next)
			continue
		}

		// Check if next files exists (yaml or sql)
		if err = m.versionUpExists(next); err != nil {
			ret <- err
			return
		}

		migr, err := m.newMigration(next, int64(next))
		if err != nil {
			ret <- err
			return
		}

		ret <- migr
		go migr.Buffer()

		from = int64(next)
	}
}

// readDownFromVersion reads down migrations from `from` limitted by `limit`. (modified version of readDown)
// limit can be -1, implying no limit and reading until there are no more migrations.
// Each migration is then written to the ret channel.
// If an error occurs during reading, that error is written to the ret channel, too.
// Once readDownFromVersion is done reading it will close the ret channel.
func (m *Migrate) readDownFromVersion(from int64, to int64, ret chan<- interface{}, bar *pb.ProgressBar) {
	defer close(ret)
	var err error
	var noOfAppliedMigrations int = 0

	if from == -1 {
		ret <- ErrNoChange
		return
	}

	for _, i := range m.status.Migrations {
		if i.Version > uint64(from) || (to >= 0 && i.Version <= uint64(to)) {
			continue
		}
		if i.IsPresent && i.IsApplied { // needs to be applied
			noOfAppliedMigrations++
		}
	}

	if noOfAppliedMigrations == 0 {
		ret <- ErrNoChange
		return
	}

	setTotalProgressBar(bar, int64(noOfAppliedMigrations))

	for {
		if m.stop() {
			return
		}

		if from == to {
			return
		}

		err = m.versionDownExists(suint64(from))
		if err != nil {
			ret <- err
			return
		}

		prev, ok := m.databaseDrv.Prev(suint64(from))
		if !ok {
			prev := new(database.MigrationVersion)
			// Check if any prev version available in source
			prev.Version, err = m.sourceDrv.Prev(suint64(from))
			if os.IsNotExist(err) && to == -1 {
				migr, err := m.newMigration(suint64(from), -1)
				if err != nil {
					ret <- err
					return
				}

				ret <- migr
				go migr.Buffer()

				from = database.NilVersion
				continue
			} else if err != nil {
				ret <- err
				return
			}
			ret <- fmt.Errorf("%v not applied on database", prev)
			return
		}

		migr, err := m.newMigration(suint64(from), int64(prev.Version))
		if err != nil {
			ret <- err
			return
		}

		ret <- migr
		go migr.Buffer()
		from = int64(prev.Version)
	}
}

func printDryRunStatus(migrations []*Migration) *bytes.Buffer {
	out := new(tabwriter.Writer)
	buf := &bytes.Buffer{}
	out.Init(buf, 0, 8, 2, ' ', 0)
	w := util.NewPrefixWriter(out)
	w.Write(util.LEVEL_0, "VERSION\tTYPE\tNAME\n")
	for _, migration := range migrations {
		var direction string
		if int64(migration.Version) == migration.TargetVersion {
			direction = "up"
		} else {
			direction = "down"
		}
		w.Write(util.LEVEL_0, "%d\t%s\t%s\n",
			migration.Version,
			direction,
			migration.Identifier,
		)
	}
	out.Flush()
	return buf
}
