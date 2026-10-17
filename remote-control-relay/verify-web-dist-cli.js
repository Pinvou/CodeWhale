import { verifyWebDist } from "./verify-web-dist.js";

// Always run: Node canonicalizes linked entry paths, so path-based main guards
// can silently skip the deployment gate. Library imports use the pure checker.
try {
  verifyWebDist(process.argv[2], process.argv[3]);
} catch {
  // Keep host paths and infrastructure details out of validation output.
  console.error("WebUI 产物校验失败：入口、base 或引用资源不完整");
  process.exitCode = 1;
}
