package smallmodel

// ExecContextNote tells a small model where a terminal command actually ran, so
// it cannot mistake a local sandbox command (root by default) for one executed
// on a remote target. Every terminal command runs in the PentAGI sandbox shell;
// only an ssh/remote wrapper reaches a target, and that remote output appears
// inline. Stamping this on each terminal result makes the execution context an
// explicit, code-managed fact instead of something the model has to infer.
const ExecContextNote = "[exec-context: PentAGI local sandbox — you are root here; a command runs on the sandbox unless you wrap it in ssh/remote, in which case the remote output appears above]"

// AppendExecContext stamps the execution-context note onto a terminal output.
func AppendExecContext(output string) string {
	return output + "\n" + ExecContextNote
}
