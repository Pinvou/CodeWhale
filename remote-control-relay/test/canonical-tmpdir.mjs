// The state store rejects symlinked path ancestors and macOS exposes its temp
// directory through the /var and /tmp symlinks, so canonicalize TMPDIR once
// for the whole run instead of repeating realpath at every fixture. This is a
// no-op on platforms whose temp directory is already canonical.
import { realpathSync } from "node:fs";
import { tmpdir } from "node:os";

process.env.TMPDIR = realpathSync(tmpdir());
