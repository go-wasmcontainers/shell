//go:build !wasip1

package wasix

// Outside GOOS=wasip1 there is no WASIX host: every call is ENOSYS, so
// that the portable parts of the shell build and test natively.

func rawSpawn(string, []string, []string, bool, []FdOp, bool, string) (Pid, error) {
	return 0, ENOSYS
}
func rawJoin(Pid, bool, JoinFlags) (Pid, Status, error) { return 0, Status{}, ENOSYS }
func rawPipe() (int, int, error)                        { return -1, -1, ENOSYS }
func rawGetpid() (Pid, error)                           { return 0, ENOSYS }
func rawGetppid(Pid) (Pid, error)                       { return 0, ENOSYS }
func rawTtyGet() (Tty, error)                           { return Tty{}, ENOSYS }
func rawTtySet(Tty) error                               { return ENOSYS }
func setNonblock(int) error                             { return ENOSYS }
func fdAlive(int) bool                                  { return false }
func setBlocking(int) error                             { return ENOSYS }
