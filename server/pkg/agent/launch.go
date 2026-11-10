package agent

import (
	"log/slog"
)

// Command is the identity of a runtime CLI: the executable Multica spawns plus
// the argv prefix that belongs to the command itself rather than to any single
// invocation.
//
// A custom runtime profile (MUL-3284) is configured as `command_name` plus
// `fixed_args`, and for a wrapper like `ccms start q36` those two fixed_args
// tokens are part of *what the program is*, not options passed to it. The
// wrapper only reaches the real Claude binary after its `start` subcommand has
// been selected, so `-p` and the rest of the stream-json protocol flags are
// meaningless — and rejected outright by a Commander-style parser — until the
// prefix has been consumed (GH #7046).
//
// So the prefix goes directly after the executable and before everything a
// backend builds:
//
//	<Path> <Prefix...> <protocol args...> <ExtraArgs...> <CustomArgs...>
//
// Subcommand-style prefixes work only in this position. Flag-style prefixes
// generally parse the same either way — `agent --model composer-2.5 -p …` and
// `agent -p … --model composer-2.5` are equivalent to most parsers — so
// prefix-first is the broader of the two orders, not a universally safe one: a
// CLI that separates global flags from subcommand flags can still care where a
// flag lands. That residual risk is why the position change is called out in
// the runtime docs rather than treated as invisible.
//
// The zero Command is a bare executable with no prefix, which is what every
// built-in runtime uses.
type Command struct {
	// Path is the executable to spawn — a PATH lookup name or an absolute path.
	Path string
	// Prefix is the argv prefix that belongs to the command, already filtered
	// by FilterLaunchPrefix. Never mutate a Command's Prefix in place; Argv
	// copies it precisely so a caller cannot alias it.
	Prefix []string
	// logger reports prefix/argument conflicts at the moment a process is
	// built. Optional: a zero Command logs nothing.
	logger *slog.Logger
}
