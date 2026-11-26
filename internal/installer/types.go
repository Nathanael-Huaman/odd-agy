package installer

const (
	ManifestSchemaVersion = 1

	ManagedHookKey = "odd-agy"
	ManagedMCPKey  = "odd-agy"
	EngramMCPKey   = "engram"

	CheckOK   = "ok"
	CheckWarn = "warn"
	CheckFail = "fail"
)

// Manifest is stored at .agents/odd/.managed-manifest.json and tracks managed workspace files and keys.
type Manifest struct {
	SchemaVersion   int               `json:"schemaVersion"`
	Version         string            `json:"version"`
	Files           map[string]string `json:"files"`
	ManagedHookKeys []string          `json:"managedHookKeys"`
	ManagedMCPKeys  []string          `json:"managedMcpKeys"`
}

// InitOptions configures project-scoped workspace installation under <workspace>/.agents/.
type InitOptions struct {
	InstallMode      string
	DelegationMode   string
	Profile          string
	BinaryCommand    string
	IncludeEngramMCP bool
	ExcludeEngramMCP bool
	Force            bool
	LookPath         func(string) (string, error)
}

// InitResult summarizes files installed and preserved during Init.
type InitResult struct {
	InstalledFiles  []string
	PreservedConfig bool
	AddedEngramMCP  bool
	ManifestPath    string
}

// SyncOptions configures managed asset synchronization under <workspace>/.agents/.
type SyncOptions struct {
	BinaryCommand string
	Force         bool
}

// SyncResult summarizes file updates, creations, conflicts, and backups during Sync.
type SyncResult struct {
	Created   []string
	Updated   []string
	Unchanged []string
	Removed   []string
	Conflicts []string
	BackedUp  []string
}

// DoctorOptions configures read-only diagnostic checks.
type DoctorOptions struct {
	HomeDir   string
	LookPath  func(string) (string, error)
	LookupEnv func(string) (string, bool)
}

// Check represents one diagnostic result from Doctor.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// DoctorReport aggregates all diagnostic checks produced by Doctor.
type DoctorReport struct {
	Checks []Check `json:"checks"`
}

// HasFailures reports whether any check has status "fail".
func (r DoctorReport) HasFailures() bool {
	for _, c := range r.Checks {
		if c.Status == CheckFail {
			return true
		}
	}
	return false
}

// HasWarnings reports whether any check has status "warn".
func (r DoctorReport) HasWarnings() bool {
	for _, c := range r.Checks {
		if c.Status == CheckWarn {
			return true
		}
	}
	return false
}

// UninstallResult summarizes removed, updated, and preserved paths during Uninstall.
type UninstallResult struct {
	Removed   []string
	Updated   []string
	Preserved []string
}
