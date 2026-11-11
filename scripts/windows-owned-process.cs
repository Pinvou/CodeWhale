// Shared Windows release-command lifetime. The child is assigned, suspended,
// to a fresh private Job before it can create descendants. No PID/tree sweep.
using System;
using System.Collections.Generic;
using System.Diagnostics;
using System.IO;
using System.Linq;
using System.Runtime.InteropServices;
using System.Text;
using System.Threading;
using System.Threading.Tasks;
using Microsoft.Win32.SafeHandles;

namespace Pinvou.Release {
    public sealed class ProcessFailure : Exception {
        public readonly int? ChildExitCode;
        public readonly bool CleanupFailed;
        public ProcessFailure(string code, int? exit, bool cleanup) : base(code) {
            ChildExitCode = exit; CleanupFailed = cleanup;
        }
    }

    public sealed class OwnedJob : IDisposable {
        internal IntPtr Handle;
        public bool IsClosed { get { return Handle == IntPtr.Zero; } }
        internal OwnedJob(IntPtr handle) { Handle = handle; }
        ~OwnedJob() { if (Handle != IntPtr.Zero) Native.CloseHandle(Handle); }
        public uint ActiveProcesses {
            get {
                Native.Accounting value;
                if (!Native.QueryInformationJobObject(Handle, 1, out value, Marshal.SizeOf(typeof(Native.Accounting)), IntPtr.Zero))
                    throw new InvalidOperationException("e2e_process_job_query_failed");
                return value.ActiveProcesses;
            }
        }
        public void Dispose() {
            if (Handle == IntPtr.Zero) return;
            bool confirmed = false;
            try {
                if (ActiveProcesses != 0 && !Native.TerminateJobObject(Handle, 86))
                    throw new InvalidOperationException("e2e_process_cleanup_failed");
                var until = Stopwatch.StartNew();
                while (until.ElapsedMilliseconds < 5000) {
                    if (ActiveProcesses == 0) { confirmed = true; break; }
                    Thread.Sleep(10);
                }
            } finally {
                // KILL_ON_JOB_CLOSE also contains interruption/host termination.
                Native.CloseHandle(Handle); Handle = IntPtr.Zero;
                GC.SuppressFinalize(this);
            }
            if (!confirmed) throw new InvalidOperationException("e2e_process_cleanup_unconfirmed");
        }
    }

    public sealed class ProcessResult {
        public int ExitCode;
        public int ProcessId;
        public string StdOut;
        public string StdErr;
        // A successful remote command may create the run's intentional service.
        // Its private Job remains owned until that same run's final cleanup.
        public OwnedJob OwnedScope;
    }

    public static class OwnedProcess {
        private sealed class Capture {
            public int Exceeded;
            public async Task<string> Drain(StreamReader reader) {
                var buffer = new char[8192];
                var text = new StringBuilder();
                int count;
                while ((count = await reader.ReadAsync(buffer, 0, buffer.Length).ConfigureAwait(false)) != 0) {
                    if (text.Length <= 16 * 1024 * 1024 - count) text.Append(buffer, 0, count);
                    else Interlocked.Exchange(ref Exceeded, 1);
                }
                return text.ToString();
            }
        }

        public static ProcessResult Run(ProcessStartInfo info, int timeoutMilliseconds,
                                        CancellationToken cancellation, bool retainSuccessfulScope) {
            if (timeoutMilliseconds < 1 || timeoutMilliseconds > 3600000)
                throw new ArgumentOutOfRangeException("timeoutMilliseconds");
            if (cancellation.IsCancellationRequested) throw new ProcessFailure("e2e_process_cancelled", null, false);
            OwnedJob job = null;
            IntPtr outRead = IntPtr.Zero, outWrite = IntPtr.Zero, errRead = IntPtr.Zero, errWrite = IntPtr.Zero;
            IntPtr input = IntPtr.Zero, attributes = IntPtr.Zero, inherited = IntPtr.Zero;
            Native.Process pi = new Native.Process();
            StreamReader output = null, errors = null;
            Task<string> stdout = null, stderr = null;
            string failure = null;
            int? exit = null;
            bool cleanupFailed = false;
            ProcessResult result = null;
            var budget = Stopwatch.StartNew();
            try {
                job = new OwnedJob(Native.Check(Native.CreateJobObjectW(IntPtr.Zero, null)));
                var limits = new Native.Extended();
                limits.BasicLimitInformation.LimitFlags = 0x2000;
                Native.Check(Native.SetInformationJobObject(job.Handle, 9, ref limits, Marshal.SizeOf(typeof(Native.Extended))));
                var sa = new Native.Security { Length = Marshal.SizeOf(typeof(Native.Security)), Inherit = 1 };
                Native.Check(Native.CreatePipe(out outRead, out outWrite, ref sa, 0));
                Native.Check(Native.CreatePipe(out errRead, out errWrite, ref sa, 0));
                Native.Check(Native.SetHandleInformation(outRead, 1, 0));
                Native.Check(Native.SetHandleInformation(errRead, 1, 0));
                input = Native.Check(Native.CreateFileW("NUL", 0x80000000, 3, ref sa, 3, 0x80, IntPtr.Zero));
                var startup = new Native.StartupEx();
                startup.Info.Size = Marshal.SizeOf(typeof(Native.StartupEx));
                startup.Info.Flags = 0x100;
                startup.Info.Input = input; startup.Info.Output = outWrite; startup.Info.Error = errWrite;
                IntPtr size = IntPtr.Zero;
                Native.InitializeProcThreadAttributeList(IntPtr.Zero, 1, 0, ref size);
                attributes = Marshal.AllocHGlobal(size);
                Native.Check(Native.InitializeProcThreadAttributeList(attributes, 1, 0, ref size));
                startup.Attributes = attributes;
                inherited = Marshal.AllocHGlobal(IntPtr.Size * 3);
                Marshal.Copy(new IntPtr[] { input, outWrite, errWrite }, 0, inherited, 3);
                Native.Check(Native.UpdateProcThreadAttribute(attributes, 0, (IntPtr)0x20002, inherited,
                    (IntPtr)(IntPtr.Size * 3), IntPtr.Zero, IntPtr.Zero));
                var environment = new StringBuilder();
                foreach (var pair in info.Environment.OrderBy(p => p.Key, StringComparer.OrdinalIgnoreCase)) {
                    if (pair.Key.IndexOf('\0') >= 0 || pair.Value.IndexOf('\0') >= 0) throw new InvalidOperationException("e2e_process_environment_invalid");
                    environment.Append(pair.Key).Append('=').Append(pair.Value).Append('\0');
                }
                environment.Append('\0');
                var command = new StringBuilder("\"" + info.FileName + "\" " + info.Arguments);
                Native.Check(Native.CreateProcessW(info.FileName, command, IntPtr.Zero, IntPtr.Zero, true,
                    0x4 | 0x400 | 0x80000 | 0x08000000, environment.ToString(),
                    String.IsNullOrEmpty(info.WorkingDirectory) ? null : info.WorkingDirectory, ref startup, out pi));
                Native.Check(Native.AssignProcessToJobObject(job.Handle, pi.ProcessHandle));
                Native.Close(ref outWrite); Native.Close(ref errWrite); Native.Close(ref input);
                output = new StreamReader(new FileStream(new SafeFileHandle(outRead, true), FileAccess.Read), Encoding.UTF8); outRead = IntPtr.Zero;
                errors = new StreamReader(new FileStream(new SafeFileHandle(errRead, true), FileAccess.Read), Encoding.UTF8); errRead = IntPtr.Zero;
                var capturedOut = new Capture(); var capturedErr = new Capture();
                stdout = capturedOut.Drain(output); stderr = capturedErr.Drain(errors);
                if (Native.ResumeThread(pi.ThreadHandle) == 0xffffffff) throw new InvalidOperationException("e2e_process_resume_failed");
                while (true) {
                    uint wait = Native.WaitForSingleObject(pi.ProcessHandle, 0);
                    if (wait == 0xffffffff) throw new InvalidOperationException("e2e_process_wait_failed");
                    if (wait == 0) {
                        uint code; Native.Check(Native.GetExitCodeProcess(pi.ProcessHandle, out code));
                        exit = unchecked((int)code);
                    }
                    if (exit.HasValue && stdout.IsCompleted && stderr.IsCompleted) break;
                    if (stdout.IsFaulted || stderr.IsFaulted) { failure = "e2e_process_pipe_failed"; break; }
                    if (cancellation.IsCancellationRequested) { failure = "e2e_process_cancelled"; break; }
                    if (budget.ElapsedMilliseconds >= timeoutMilliseconds) { failure = "e2e_process_timeout"; break; }
                    if (capturedOut.Exceeded != 0 || capturedErr.Exceeded != 0) { failure = "e2e_process_output_limit"; break; }
                    Thread.Sleep(10);
                }
                if (failure == null && (capturedOut.Exceeded != 0 || capturedErr.Exceeded != 0)) failure = "e2e_process_output_limit";
                if (failure == null) {
                    // GetResult observes decoder/pipe failures rather than using partial JSON.
                    result = new ProcessResult { ExitCode = exit.Value, ProcessId = unchecked((int)pi.ProcessId),
                        StdOut = stdout.GetAwaiter().GetResult(), StdErr = stderr.GetAwaiter().GetResult() };
                    if (exit == 0 && retainSuccessfulScope && job.ActiveProcesses != 0) {
                        result.OwnedScope = job; job = null;
                    }
                }
            } catch (Exception) {
                if (failure == null) failure = "e2e_process_execution_failed";
            } finally {
                // Before assignment succeeds, only the exact suspended child exists.
                if (pi.ProcessHandle != IntPtr.Zero && job != null) {
                    Native.TerminateProcess(pi.ProcessHandle, 86);
                    if (Native.WaitForSingleObject(pi.ProcessHandle, 5000) != 0) cleanupFailed = true;
                }
                if (job != null) { try { job.Dispose(); } catch { cleanupFailed = true; } }
                if (stdout != null && stderr != null) {
                    try { if (!Task.WaitAll(new Task[] { stdout, stderr }, 5000)) cleanupFailed = true; }
                    catch { if (failure == null) failure = "e2e_process_pipe_failed"; }
                }
                try { if (output != null) output.Dispose(); } catch { cleanupFailed = true; }
                try { if (errors != null) errors.Dispose(); } catch { cleanupFailed = true; }
                Native.Close(ref outRead); Native.Close(ref outWrite); Native.Close(ref errRead); Native.Close(ref errWrite); Native.Close(ref input);
                Native.Close(ref pi.ThreadHandle); Native.Close(ref pi.ProcessHandle);
                if (attributes != IntPtr.Zero) { Native.DeleteProcThreadAttributeList(attributes); Marshal.FreeHGlobal(attributes); }
                if (inherited != IntPtr.Zero) Marshal.FreeHGlobal(inherited);
            }
            if (failure != null || cleanupFailed) {
                if (result != null && result.OwnedScope != null) {
                    try { result.OwnedScope.Dispose(); } catch { cleanupFailed = true; }
                }
                throw new ProcessFailure(exit.HasValue && exit.Value != 0 ? "e2e_process_nonzero_exit" : failure ?? "e2e_process_cleanup_failed", exit, cleanupFailed);
            }
            return result;
        }
    }

    internal static class Native {
        [StructLayout(LayoutKind.Sequential)] internal struct Security { public int Length; public IntPtr Descriptor; public int Inherit; }
        [StructLayout(LayoutKind.Sequential, CharSet=CharSet.Unicode)] internal struct Startup {
            public int Size; public string Reserved, Desktop, Title; public int X,Y,XSize,YSize,XCount,YCount,Fill,Flags;
            public short Show, ReservedSize; public IntPtr ReservedBytes, Input, Output, Error;
        }
        [StructLayout(LayoutKind.Sequential)] internal struct StartupEx { public Startup Info; public IntPtr Attributes; }
        [StructLayout(LayoutKind.Sequential)] internal struct Process { public IntPtr ProcessHandle, ThreadHandle; public uint ProcessId, ThreadId; }
        [StructLayout(LayoutKind.Sequential)] internal struct Basic { public long ProcessTime,JobTime; public uint LimitFlags; public UIntPtr Min,Max; public uint Active; public UIntPtr Affinity; public uint Priority,Scheduling; }
        [StructLayout(LayoutKind.Sequential)] internal struct Io { public ulong Read,Write,Other,ReadBytes,WriteBytes,OtherBytes; }
        [StructLayout(LayoutKind.Sequential)] internal struct Extended { public Basic BasicLimitInformation; public Io IoInfo; public UIntPtr ProcessMemory,JobMemory,PeakProcess,PeakJob; }
        [StructLayout(LayoutKind.Sequential)] internal struct Accounting { public long User,Kernel,PeriodUser,PeriodKernel; public uint Faults,TotalProcesses,ActiveProcesses,TerminatedProcesses; }
        internal static IntPtr Check(IntPtr value) { if (value == IntPtr.Zero || value == (IntPtr)(-1)) throw new InvalidOperationException("e2e_process_native_failed"); return value; }
        internal static void Check(bool value) { if (!value) throw new InvalidOperationException("e2e_process_native_failed"); }
        internal static void Close(ref IntPtr value) { if (value != IntPtr.Zero && value != (IntPtr)(-1)) CloseHandle(value); value=IntPtr.Zero; }
        [DllImport("kernel32.dll",CharSet=CharSet.Unicode,SetLastError=true)] internal static extern IntPtr CreateJobObjectW(IntPtr sa,string name);
        [DllImport("kernel32.dll",SetLastError=true)] internal static extern bool SetInformationJobObject(IntPtr job,int kind,ref Extended value,int length);
        [DllImport("kernel32.dll",SetLastError=true)] internal static extern bool QueryInformationJobObject(IntPtr job,int kind,out Accounting value,int length,IntPtr returned);
        [DllImport("kernel32.dll",SetLastError=true)] internal static extern bool AssignProcessToJobObject(IntPtr job,IntPtr process);
        [DllImport("kernel32.dll",SetLastError=true)] internal static extern bool TerminateJobObject(IntPtr job,uint code);
        [DllImport("kernel32.dll",SetLastError=true)] internal static extern bool CreatePipe(out IntPtr read,out IntPtr write,ref Security sa,uint size);
        [DllImport("kernel32.dll",SetLastError=true)] internal static extern bool SetHandleInformation(IntPtr handle,uint mask,uint flags);
        [DllImport("kernel32.dll",CharSet=CharSet.Unicode,SetLastError=true)] internal static extern IntPtr CreateFileW(string name,uint access,uint share,ref Security sa,uint creation,uint flags,IntPtr template);
        [DllImport("kernel32.dll",SetLastError=true)] internal static extern bool InitializeProcThreadAttributeList(IntPtr list,int count,int flags,ref IntPtr size);
        [DllImport("kernel32.dll",SetLastError=true)] internal static extern bool UpdateProcThreadAttribute(IntPtr list,uint flags,IntPtr attribute,IntPtr value,IntPtr size,IntPtr previous,IntPtr returned);
        [DllImport("kernel32.dll")] internal static extern void DeleteProcThreadAttributeList(IntPtr list);
        [DllImport("kernel32.dll",CharSet=CharSet.Unicode,SetLastError=true)] internal static extern bool CreateProcessW(string app,StringBuilder command,IntPtr processSa,IntPtr threadSa,bool inherit,uint flags,string environment,string directory,ref StartupEx startup,out Process process);
        [DllImport("kernel32.dll",SetLastError=true)] internal static extern uint ResumeThread(IntPtr thread);
        [DllImport("kernel32.dll",SetLastError=true)] internal static extern uint WaitForSingleObject(IntPtr handle,uint milliseconds);
        [DllImport("kernel32.dll",SetLastError=true)] internal static extern bool GetExitCodeProcess(IntPtr process,out uint code);
        [DllImport("kernel32.dll",SetLastError=true)] internal static extern bool TerminateProcess(IntPtr process,uint code);
        [DllImport("kernel32.dll")] internal static extern bool CloseHandle(IntPtr handle);
    }
}
