using System;
using System.IO;
using System.Threading;

// Compatible with Windows PowerShell 5.1/.NET Framework. The cancellation
// callback is managed code: a timer thread must never invoke a PowerShell
// scriptblock without a runspace. Disposing the input also interrupts streams
// whose ReadAsync implementation ignores its CancellationToken.
namespace Pinvou.RuntimeRelease
{
    public static class Body
    {
        public static void Copy(Stream input, Stream output, long expectedBytes,
                                CancellationToken cancellation)
        {
            if (expectedBytes <= 0 || expectedBytes >= 2L * 1024 * 1024 * 1024)
                throw new ArgumentOutOfRangeException("expectedBytes");
            using (cancellation.Register(delegate {
                try { input.Dispose(); }
                catch { /* Cancellation remains a failure even if close fails. */ }
            }))
            {
                byte[] buffer = new byte[81920];
                long total = 0;
                while (true)
                {
                    cancellation.ThrowIfCancellationRequested();
                    // Probe one extra byte, but never write beyond the lock.
                    int limit = (int)Math.Min(buffer.Length, expectedBytes - total + 1);
                    var read = input.ReadAsync(buffer, 0, limit, cancellation);
                    try { read.Wait(cancellation); }
                    catch {
                        // Observe a late failure when an implementation ignores
                        // cancellation. The caller closes both streams in finally.
                        read.ContinueWith(t => { var ignored = t.Exception; },
                            System.Threading.Tasks.TaskContinuationOptions.OnlyOnFaulted);
                        throw;
                    }
                    int count = read.GetAwaiter().GetResult();
                    cancellation.ThrowIfCancellationRequested();
                    if (count == 0) break;
                    if (count > expectedBytes - total)
                        throw new InvalidDataException("Runtime Release body exceeds the locked size.");
                    output.Write(buffer, 0, count);
                    total += count;
                }
                cancellation.ThrowIfCancellationRequested();
                if (total != expectedBytes)
                    throw new InvalidDataException("Runtime Release body differs from the locked size.");
            }
        }
    }
}
