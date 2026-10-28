using System;
using System.ComponentModel;
using System.Runtime.InteropServices;
using System.Text;

public static class PinvouInstallHandoff {
    [StructLayout(LayoutKind.Sequential, CharSet=CharSet.Unicode)] struct StartupInfo { public int cb; public string reserved, desktop, title; public int x,y,xsize,ysize,xcount,ycount,fill,flags; public short show,reserved2; public IntPtr reservedPtr, stdin, stdout, stderr; }
    [StructLayout(LayoutKind.Sequential)] struct StartupInfoEx { public StartupInfo startup; public IntPtr attributes; }
    [StructLayout(LayoutKind.Sequential)] struct ProcessInfo { public IntPtr process, thread; public uint pid, tid; }
    [StructLayout(LayoutKind.Sequential)] struct BasicLimit { public long processTime, jobTime; public uint flags; public UIntPtr minWorking, maxWorking; public uint activeLimit; public UIntPtr affinity; public uint priority, scheduling; }
    [StructLayout(LayoutKind.Sequential)] struct IoCounters { public ulong a,b,c,d,e,f; }
    [StructLayout(LayoutKind.Sequential)] struct ExtendedLimit { public BasicLimit basic; public IoCounters io; public UIntPtr processMemory, jobMemory, peakProcess, peakJob; }
    [DllImport("kernel32.dll", SetLastError=true)] static extern IntPtr OpenProcess(uint access, bool inherit, uint pid);
    [DllImport("kernel32.dll", SetLastError=true)] static extern bool GetProcessTimes(IntPtr p, out long created, out long exited, out long kernel, out long user);
    [DllImport("kernel32.dll", CharSet=CharSet.Unicode, SetLastError=true)] static extern bool CreateProcess(string app, StringBuilder command, IntPtr pa, IntPtr ta, bool inherit, uint flags, IntPtr env, string dir, ref StartupInfoEx si, out ProcessInfo pi);
    [DllImport("kernel32.dll", CharSet=CharSet.Unicode, SetLastError=true)] static extern IntPtr CreateJobObject(IntPtr attributes, string name);
    [DllImport("kernel32.dll", SetLastError=true)] static extern bool SetInformationJobObject(IntPtr job, int type, ref ExtendedLimit info, uint length);
    [DllImport("kernel32.dll", SetLastError=true)] static extern bool InitializeProcThreadAttributeList(IntPtr list, int count, int flags, ref IntPtr size);
    [DllImport("kernel32.dll", SetLastError=true)] static extern bool UpdateProcThreadAttribute(IntPtr list, uint flags, IntPtr attribute, IntPtr value, IntPtr size, IntPtr previous, IntPtr returned);
    [DllImport("kernel32.dll")] static extern void DeleteProcThreadAttributeList(IntPtr list);
    [DllImport("kernel32.dll", SetLastError=true)] static extern uint WaitForSingleObject(IntPtr handle, uint milliseconds);
    [DllImport("kernel32.dll", SetLastError=true)] static extern uint ResumeThread(IntPtr thread);
    [DllImport("kernel32.dll", SetLastError=true)] static extern bool TerminateProcess(IntPtr process, uint code);
    [DllImport("kernel32.dll")] static extern bool CloseHandle(IntPtr handle);
    static void Check(bool ok) { if (!ok) throw new Win32Exception(Marshal.GetLastWin32Error()); }
    public static void Run(uint oldPid, long oldCreated, string executable, string arguments, string started, string approved, string nonce, uint waitMilliseconds) {
        IntPtr old = IntPtr.Zero, job = IntPtr.Zero, attributes = IntPtr.Zero, jobList = IntPtr.Zero; ProcessInfo pi = new ProcessInfo(); bool initialized = false;
        try {
            old = OpenProcess(0x100000 | 0x1000, false, oldPid);
            if (old == IntPtr.Zero) throw new Win32Exception(Marshal.GetLastWin32Error());
            long creation, exit, kernel, user; Check(GetProcessTimes(old, out creation, out exit, out kernel, out user));
            if (creation != oldCreated || WaitForSingleObject(old, 0) != 0x102) throw new InvalidOperationException("original application process is no longer running");
            job = CreateJobObject(IntPtr.Zero, null); if (job == IntPtr.Zero) throw new Win32Exception(Marshal.GetLastWin32Error());
            ExtendedLimit limit = new ExtendedLimit(); limit.basic.flags = 0x2000; Check(SetInformationJobObject(job, 9, ref limit, (uint)Marshal.SizeOf(typeof(ExtendedLimit))));
            IntPtr size = IntPtr.Zero; InitializeProcThreadAttributeList(IntPtr.Zero, 1, 0, ref size);
            attributes = Marshal.AllocHGlobal(size); Check(InitializeProcThreadAttributeList(attributes, 1, 0, ref size)); initialized = true;
            jobList = Marshal.AllocHGlobal(IntPtr.Size); Marshal.WriteIntPtr(jobList, job);
            Check(UpdateProcThreadAttribute(attributes, 0, new IntPtr(0x0002000d), jobList, new IntPtr(IntPtr.Size), IntPtr.Zero, IntPtr.Zero));
            StartupInfoEx si = new StartupInfoEx(); si.startup.cb = Marshal.SizeOf(typeof(StartupInfoEx)); si.attributes = attributes;
            Check(CreateProcess(executable, new StringBuilder("\"" + executable + "\" " + arguments), IntPtr.Zero, IntPtr.Zero, false, 0x00080004, IntPtr.Zero, null, ref si, out pi));
            using (System.Diagnostics.Process self = System.Diagnostics.Process.GetCurrentProcess()) {
                long selfCreation, selfExit, selfKernel, selfUser;
                Check(GetProcessTimes(self.Handle, out selfCreation, out selfExit, out selfKernel, out selfUser));
                System.IO.File.WriteAllText(started, nonce + ":" + pi.pid + ":" + self.Id + ":" + selfCreation + ":ready", new UTF8Encoding(false));
            }
            uint wait = WaitForSingleObject(old, waitMilliseconds);
            if (wait != 0) throw new InvalidOperationException("application exit was not observed (wait " + wait + ")");
            if (!System.IO.File.Exists(approved) || System.IO.File.ReadAllText(approved) != nonce) throw new InvalidOperationException("application did not confirm installation handoff");
            System.IO.File.Delete(approved);
            if (ResumeThread(pi.thread) == 0xffffffff) throw new Win32Exception(Marshal.GetLastWin32Error());
            limit.basic.flags = 0; Check(SetInformationJobObject(job, 9, ref limit, (uint)Marshal.SizeOf(typeof(ExtendedLimit))));
            if (WaitForSingleObject(pi.process, 0xffffffff) != 0) throw new Win32Exception(Marshal.GetLastWin32Error());
        } finally {
            if (initialized) DeleteProcThreadAttributeList(attributes);
            if (attributes != IntPtr.Zero) Marshal.FreeHGlobal(attributes);
            if (jobList != IntPtr.Zero) Marshal.FreeHGlobal(jobList);
            if (pi.thread != IntPtr.Zero) CloseHandle(pi.thread);
            if (pi.process != IntPtr.Zero) CloseHandle(pi.process);
            if (job != IntPtr.Zero) CloseHandle(job);
            if (old != IntPtr.Zero) CloseHandle(old);
        }
    }
}
