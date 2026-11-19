// Executable protocol seam for the real staging/guard/runspace/Job regression.
// It is NOT the signed release driver, an attestation provider or a gateway.
using System;
using System.Collections.Generic;
using System.Diagnostics;
using System.IO;
using System.Linq;
using System.Runtime.InteropServices;
using System.Security.Cryptography;
using System.Text;
using System.Threading;
using System.Web.Script.Serialization;

public sealed class ChildRecord {
    public int pid;
    public long creation_ticks;
    public string executable;
    public string sha256;
    public string kind;
    public bool terminated;
}
public sealed class DriverRegistry {
    public string nonce;
    public List<ChildRecord> children = new List<ChildRecord>();
}
public static class OwnedDriverFixture {
    [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
    private struct Startup {
        public int Size; public string Reserved, Desktop, Title;
        public int X, Y, XSize, YSize, XCount, YCount, Fill, Flags;
        public short Show, ReservedSize; public IntPtr ReservedBytes, Input, Output, Error;
    }
    [StructLayout(LayoutKind.Sequential)]
    private struct Started { public IntPtr Process, Thread; public uint ProcessId, ThreadId; }
    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern bool CreateProcessW(string application, StringBuilder command,
        IntPtr processAttributes, IntPtr threadAttributes, bool inheritHandles, uint flags,
        IntPtr environment, string directory, ref Startup startup, out Started started);
    [DllImport("kernel32.dll")]
    private static extern bool CloseHandle(IntPtr handle);
    private static readonly JavaScriptSerializer Json = new JavaScriptSerializer();
    private static readonly string Root = AppDomain.CurrentDomain.BaseDirectory;
    private static readonly string Ledger = Path.Combine(Root, "fixture-owned-processes.json");
    private static string Arg(string[] args, string key, string fallback) {
        int i = Array.IndexOf(args, key);
        return i >= 0 && i + 1 < args.Length ? args[i + 1] : fallback;
    }
    private static string Hash(string path) {
        using (var input = File.OpenRead(path)) using (var hash = SHA256.Create())
            return BitConverter.ToString(hash.ComputeHash(input)).Replace("-", "").ToLowerInvariant();
    }
    private static void Save(DriverRegistry value) {
        string temporary = Ledger + ".tmp";
        File.WriteAllText(temporary, Json.Serialize(value), new UTF8Encoding(false));
        if (File.Exists(Ledger)) File.Replace(temporary, Ledger, null);
        else File.Move(temporary, Ledger);
    }
    private static void Print(object value) { Console.Write(Json.Serialize(value)); }
    public static int Main(string[] args) {
        if (args.Length == 1 && args[0] == "--fixture-service") {
            File.WriteAllText(Path.Combine(Root, "fixture-service-" + Process.GetCurrentProcess().Id + ".ready"), "ready");
            Thread.Sleep(Timeout.Infinite);
            return 0;
        }
        Console.OutputEncoding = new UTF8Encoding(false);
        try {
            File.AppendAllText(Path.Combine(Root, "fixture-driver-phase.log"), "entered\n");
            var registry = File.Exists(Ledger) ? Json.Deserialize<DriverRegistry>(File.ReadAllText(Ledger)) : new DriverRegistry();
            string nonce = Arg(args, "--run-nonce", registry.nonce);
            if (registry.nonce != null && nonce != registry.nonce) throw new Exception("fixture_owner_mismatch");
            registry.nonce = nonce;
            if (args[0] == "legacy-start" || args[0] == "start-role") {
                string executable = Path.Combine(Root, "legacy-relay.exe");
                // A normal long-lived producer must detach its IO as well as
                // its command root. Inherit no launcher pipe handles. Windows
                // still assigns this child to the existing private nested Job.
                var startup = new Startup { Size = Marshal.SizeOf(typeof(Startup)) };
                Started started;
                if (!CreateProcessW(executable, new StringBuilder("\"" + executable + "\" --fixture-service"),
                    IntPtr.Zero, IntPtr.Zero, false, 0x08000000, IntPtr.Zero, Root, ref startup, out started))
                    throw new Exception("fixture_service_start_failed");
                CloseHandle(started.Thread);
                try { using (var child = Process.GetProcessById((int)started.ProcessId)) {
                    var ready = Path.Combine(Root, "fixture-service-" + child.Id + ".ready");
                    var wait = Stopwatch.StartNew();
                    while (!File.Exists(ready)) {
                        if (child.HasExited || wait.ElapsedMilliseconds >= 3000)
                            throw new Exception("fixture_service_not_ready");
                        Thread.Sleep(10);
                    }
                    File.AppendAllText(Path.Combine(Root, "fixture-driver-phase.log"), "child-started\n");
                    registry.children.Add(new ChildRecord { pid = child.Id,
                        creation_ticks = child.StartTime.ToUniversalTime().Ticks,
                        executable = executable, sha256 = Hash(executable),
                        kind = args[0] == "legacy-start" ? "legacy" : "role" });
                    // Durable ownership precedes the successful response.
                    Save(registry);
                    File.AppendAllText(Path.Combine(Root, "fixture-driver-phase.log"), "ownership-saved\n");
                    Print(new { schema = "pinvou-fixture-runtime", pid = child.Id, mock = false });
                    File.AppendAllText(Path.Combine(Root, "fixture-driver-phase.log"), "response-written\n");
                } } finally { CloseHandle(started.Process); }
            } else if (args[0] == "legacy-owned-pids") {
                Print(new { schema = "pinvou-legacy-owned-processes", version = 1,
                    run_nonce = nonce, mock = false,
                    active = registry.children.Where(c => c.kind == "legacy" && !c.terminated).Select(c => c.pid).ToArray(),
                    terminated = registry.children.Where(c => c.kind == "legacy" && c.terminated).Select(c => c.pid).ToArray() });
            } else if (args[0] == "legacy-stop" || args[0] == "stop-role") {
                string kind = args[0] == "legacy-stop" ? "legacy" : "role";
                foreach (var record in registry.children.Where(c => c.kind == kind && !c.terminated)) {
                    using (var child = Process.GetProcessById(record.pid)) {
                        // Pin the exact process object before validating or
                        // terminating; never sweep by name or a bare PID.
                        IntPtr pinned = child.Handle;
                        if (child.StartTime.ToUniversalTime().Ticks != record.creation_ticks
                            || !String.Equals(child.MainModule.FileName, record.executable, StringComparison.OrdinalIgnoreCase)
                            || Hash(record.executable) != record.sha256) throw new Exception("fixture_child_identity_mismatch");
                        child.Kill();
                        if (!child.WaitForExit(5000)) throw new Exception("fixture_child_exit_unconfirmed");
                        GC.KeepAlive(pinned);
                    }
                    record.terminated = true;
                }
                Save(registry);
            } else if (args[0] == "legacy-deconfigure-gateway") {
                // Explicit network seam; the real final-cleanup consumer runs.
                string original = Arg(args, "--restore-original-state", "original");
                Print(new { owned_route_removed = true, public_route_withdrawn = true,
                    original_state_digest = original, restored_state_digest = original,
                    control_plane_state_after_digest = Arg(args, "--expected-control-plane-state-digest", "before") });
            } else throw new Exception("fixture_command_unknown");
            return 0;
        } catch {
            Console.Error.Write("fixture_driver_failed");
            return 17;
        }
    }
}
