using System;
using System.IO;
using System.Net;
using System.Net.Http;
using System.Net.Sockets;
using System.Text;
using System.Threading;
using System.Threading.Tasks;

namespace Pinvou.RuntimeRelease.Tests
{
    // Only the HTTP boundary is replaced. Production URI checks, redirects,
    // deadline, bounded copying and disposal still execute in Write-Download.
    public sealed class Handler : HttpMessageHandler
    {
        public string Mode;
        public int Requests;
        public bool Disposed;
        public BodyStream Body;
        public Handler(string mode) { Mode = mode; }
        protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken token)
        {
            Requests++;
            if (Mode == "headers-stall") await Task.Delay(Timeout.Infinite, token);
            if (Mode == "redirect" || Mode == "unsafe" || Mode == "loop")
            {
                if (Requests == 1 || Mode == "loop")
                {
                    var redirect = new HttpResponseMessage(HttpStatusCode.Found);
                    redirect.Headers.Location = new Uri(Mode == "unsafe" ? "http://example.invalid/a" : "https://release-assets.githubusercontent.com/a");
                    return redirect;
                }
            }
            var response = new HttpResponseMessage(Mode == "404" ? HttpStatusCode.NotFound : HttpStatusCode.OK);
            Body = new BodyStream(Mode == "oversize" ? "abcdef" : Mode == "short" ? "ab" : "abc", Mode == "body-stall");
            response.Content = new StreamContent(Body);
            return response;
        }
        protected override void Dispose(bool disposing) { Disposed = true; base.Dispose(disposing); }
    }

    public sealed class BodyStream : Stream
    {
        readonly byte[] bytes;
        readonly bool stall;
        readonly TaskCompletionSource<int> pending = new TaskCompletionSource<int>();
        int position;
        public bool Disposed;
        public BodyStream(string text, bool stall) { bytes = Encoding.ASCII.GetBytes(text); this.stall = stall; }
        public override bool CanRead { get { return true; } }
        public override bool CanSeek { get { return false; } }
        public override bool CanWrite { get { return false; } }
        public override long Length { get { throw new NotSupportedException(); } }
        public override long Position { get { return position; } set { throw new NotSupportedException(); } }
        public override int Read(byte[] buffer, int offset, int count)
        {
            if (Disposed) throw new ObjectDisposedException("body");
            int copied = Math.Min(count, bytes.Length - position);
            Array.Copy(bytes, position, buffer, offset, copied); position += copied; return copied;
        }
        public override Task<int> ReadAsync(byte[] buffer, int offset, int count, CancellationToken token)
        {
            // Deliberately ignores cancellation, like older network streams.
            if (stall) return pending.Task;
            return Task.FromResult(Read(buffer, offset, count));
        }
        protected override void Dispose(bool disposing)
        {
            Disposed = true;
            pending.TrySetException(new ObjectDisposedException("body"));
            base.Dispose(disposing);
        }
        public override void Flush() { }
        public override long Seek(long offset, SeekOrigin origin) { throw new NotSupportedException(); }
        public override void SetLength(long value) { throw new NotSupportedException(); }
        public override void Write(byte[] buffer, int offset, int count) { throw new NotSupportedException(); }
    }

    // Real socket/HttpClient response stream, not a replacement for the body
    // algorithm. Test-only loopback is never permitted by the production URI policy.
    public sealed class Loopback : IDisposable
    {
        readonly TcpListener listener = new TcpListener(IPAddress.Loopback, 0);
        readonly CancellationTokenSource stop = new CancellationTokenSource();
        readonly Task worker;
        TcpClient peer;
        public readonly Uri Uri;
        public Loopback()
        {
            listener.Start();
            Uri = new Uri("http://127.0.0.1:" + ((IPEndPoint)listener.LocalEndpoint).Port + "/");
            worker = Task.Run(async delegate {
                peer = await listener.AcceptTcpClientAsync();
                using (peer)
                using (var stream = peer.GetStream())
                {
                    // Consume the actual request before returning a partial chunked body.
                    int matched = 0; byte[] one = new byte[1]; byte[] end = {13, 10, 13, 10};
                    while (matched < 4)
                    {
                        int n = await stream.ReadAsync(one, 0, 1, stop.Token);
                        if (n == 0) throw new IOException("Request ended before headers");
                        matched = one[0] == end[matched] ? matched + 1 : (one[0] == 13 ? 1 : 0);
                    }
                    var headers = Encoding.ASCII.GetBytes("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n1\r\na\r\n");
                    await stream.WriteAsync(headers, 0, headers.Length, stop.Token);
                    try { await Task.Delay(Timeout.Infinite, stop.Token); }
                    catch (OperationCanceledException) { }
                }
            });
        }
        public void Dispose()
        {
            stop.Cancel(); listener.Stop();
            if (peer != null) peer.Close();
            try {
                if (!worker.Wait(TimeSpan.FromSeconds(5))) throw new TimeoutException("Loopback server did not retire");
                worker.GetAwaiter().GetResult();
            }
            finally { stop.Dispose(); }
        }
    }
}
