using System.Diagnostics;
using System.Runtime.InteropServices;
using System.Security.Cryptography;
using System.Text;
using Microsoft.Win32.SafeHandles;

namespace Cage.Windows;

internal static class UpdateRecovery
{
    public static string BackendPath(string backup) => Path.Combine(backup, "resources", "cage.exe");

    // prepare-update returns once the controller has released its profile lock.
    // The controller, its log relay and runtime children (proxy core, QEMU) can
    // still be exiting then and keep their images below the installation mapped.
    // Wait for exactly those processes: each is identified by the image path of
    // a handle opened once, and that handle pins the process object, so a reused
    // PID can never stand in for it. Nothing is terminated; on timeout the
    // caller fails before changing the installation.
    public static async Task WaitForInstallationProcesses(string target, TimeSpan timeout)
    {
        if (!OperatingSystem.IsWindows()) return;
        var root = LongPath(Path.GetFullPath(target)).TrimEnd(Path.DirectorySeparatorChar) + Path.DirectorySeparatorChar;
        var running = new List<(int Pid, string Image, SafeProcessHandle Handle)>();
        try
        {
            foreach (var process in Process.GetProcesses())
            {
                using (process)
                {
                    if (process.Id == Environment.ProcessId || process.Id <= 4) continue;
                    // Other users' and protected processes cannot be opened. They
                    // cannot run this user's installation; NSIS still checks it.
                    var handle = OpenProcess(ProcessQueryLimitedInformation | Synchronize, false, process.Id);
                    if (handle.IsInvalid) { handle.Dispose(); continue; }
                    var image = ImagePath(handle) is { } path ? LongPath(path) : null;
                    if (image != null && image.StartsWith(root, StringComparison.OrdinalIgnoreCase) &&
                        WaitForSingleObject(handle, 0) == WaitTimeout)
                        running.Add((process.Id, image, handle));
                    else handle.Dispose();
                }
            }
            var deadline = DateTime.UtcNow + timeout;
            foreach (var (pid, image, handle) in running)
            {
                var remaining = (uint)Math.Clamp((deadline - DateTime.UtcNow).TotalMilliseconds, 0, int.MaxValue);
                if (await Task.Run(() => WaitForSingleObject(handle, remaining)) != 0)
                    throw new IOException("安装目录中的 Cage 进程仍未退出（" + Path.GetRelativePath(root, image) + "，PID " + pid + "），未启动安装程序");
            }
        }
        finally { foreach (var entry in running) entry.Handle.Dispose(); }
    }

    private static string? ImagePath(SafeProcessHandle process)
    {
        var buffer = new StringBuilder(32768);
        var size = (uint)buffer.Capacity;
        return QueryFullProcessImageNameW(process, 0, buffer, ref size) ? buffer.ToString(0, (int)size) : null;
    }

    // Compare installation paths independent of 8.3 short-name components.
    private static string LongPath(string path)
    {
        var buffer = new StringBuilder(32768);
        var length = GetLongPathNameW(path, buffer, (uint)buffer.Capacity);
        return length > 0 && length < buffer.Capacity ? buffer.ToString() : path;
    }

    private const uint ProcessQueryLimitedInformation = 0x1000, Synchronize = 0x100000, WaitTimeout = 0x102;
    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern SafeProcessHandle OpenProcess(uint access, bool inherit, int processId);
    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern bool QueryFullProcessImageNameW(SafeProcessHandle process, uint flags, StringBuilder name, ref uint size);
    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern uint GetLongPathNameW(string path, StringBuilder buffer, uint length);
    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern uint WaitForSingleObject(SafeProcessHandle handle, uint milliseconds);

    // NSIS may reject an update or restore its original directory before it
    // exits. A started installer alone does not mean the App was replaced.
    public static bool OriginalIntact(string target, string backup)
    {
        try
        {
            if (File.Exists(target + ".cage-installation.json") || !Directory.Exists(target) || !Directory.Exists(backup)) return false;
            UpdatePolicy.PlainPath(target); UpdatePolicy.PlainPath(backup);
            if (!File.Exists(Path.Combine(backup, "Cage.exe")) || !File.Exists(Path.Combine(backup, "Cage.install-id"))) return false;
            foreach (var source in Directory.EnumerateFiles(backup, "*", SearchOption.AllDirectories))
            {
                var installed = Path.Combine(target, Path.GetRelativePath(backup, source));
                UpdatePolicy.PlainPath(source); UpdatePolicy.PlainPath(installed);
                using var before = File.OpenRead(source);
                using var after = File.OpenRead(installed);
                if (before.Length != after.Length || !SHA256.HashData(before).AsSpan().SequenceEqual(SHA256.HashData(after))) return false;
            }
            return true;
        }
        catch (IOException) { return false; }
        catch (UnauthorizedAccessException) { return false; }
    }

    // The installed candidate may be unable to start. Use the existing backup's
    // verified controller to check the shared profile and stop any new process.
    public static async Task PrepareRollback(string backup, CancellationToken cancellation = default)
    {
        var (exit, result) = await Backend.JsonAt(BackendPath(backup), ["prepare-update", "--json", "--stop"], cancellation);
        if (exit != 0 || result.Text("status") != "prepared")
            throw new IOException("旧版后台无法确认环境已停止，原应用备份已保留");
    }

    public static void CleanupDirectory(string? path)
    {
        if (path == null) return;
        try { Directory.Delete(path, true); }
        catch (IOException) { }
        catch (UnauthorizedAccessException) { }
    }

    // This only runs after workbench acknowledgement. Cleanup failures leave
    // files for later removal and cannot change the installation result.
    public static void CleanupAfterSuccess(string backup, string download, string marker)
    {
        CleanupDirectory(backup);
        CleanupDirectory(download);
        try { File.Delete(marker); }
        catch (IOException) { }
        catch (UnauthorizedAccessException) { }
    }
}
