//go:build linux

package author

import "github.com/JunYupK/JunKloud/internal/environment"

// Compatibility names; all ownership and deletion rules live in environment.
type Janitor = environment.Janitor
type ClusterJanitor = environment.ClusterJanitor
type JanitorOption = environment.JanitorOption

var ErrCleanupTimeout = environment.ErrCleanupTimeout
var WithCleanupTimeout = environment.WithCleanupTimeout
var WithCleanupPoll = environment.WithCleanupPoll
var NewJanitor = environment.NewJanitor
var NewClusterJanitor = environment.NewClusterJanitor
