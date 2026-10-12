// Keep the production Compose environment regression in the existing Node
// test discovery used by npm test. The imported suite models merge semantics;
// it does not claim that Docker or a deployed private environment was run.
import '../../scripts/tests/account-platform-invocation-environment.test.mjs';
