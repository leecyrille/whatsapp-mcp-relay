# Watches one taskbar or tray button's pixels. Reads the screen only: it clicks nothing and saves no pictures.
# Prints one JSON line. Used by taskbar-real.mjs.
param([string]$Title, [double]$Seconds = 0, [int]$AppPid = 0)
$src = @'
using System;
using System.Collections.Generic;
using System.Drawing;
using System.Runtime.InteropServices;
using System.Threading;
using System.Windows.Automation;

public static class Watch {
  [DllImport("user32.dll")] static extern IntPtr GetForegroundWindow();
  [DllImport("user32.dll")] static extern uint GetWindowThreadProcessId(IntPtr h, out uint pid);
  [DllImport("user32.dll")] static extern uint GetGuiResources(IntPtr proc, uint flags);
  [DllImport("user32.dll")] static extern bool GetCursorPos(out Point p);
  [StructLayout(LayoutKind.Sequential)] struct LASTINPUTINFO { public uint cbSize; public uint dwTime; }
  [DllImport("user32.dll")] static extern bool GetLastInputInfo(ref LASTINPUTINFO i);
  static double InputAge() { var i = new LASTINPUTINFO { cbSize = 8 }; GetLastInputInfo(ref i); return (Environment.TickCount - (int)i.dwTime) / 1000.0; }

  static Rectangle Find(string title) {
    var tray = AutomationElement.RootElement.FindFirst(TreeScope.Children, new PropertyCondition(AutomationElement.ClassNameProperty, "Shell_TrayWnd"));
    if (tray == null) return Rectangle.Empty;
    foreach (AutomationElement e in tray.FindAll(TreeScope.Descendants, Condition.TrueCondition)) {
      try {
        if ((e.Current.Name ?? "").IndexOf(title, StringComparison.OrdinalIgnoreCase) >= 0) {
          var r = e.Current.BoundingRectangle;
          if (r.Width > 0 && r.Height > 0) return new Rectangle((int)r.X, (int)r.Y, (int)r.Width, (int)r.Height);
        }
      } catch {}
    }
    return Rectangle.Empty;
  }

  static int ForegroundPid() { uint p; GetWindowThreadProcessId(GetForegroundWindow(), out p); return (int)p; }

  public static string Run(string title, double seconds, int appPid) {
    var gdi = 0u; var user = 0u;
    if (appPid != 0) {
      try {
        var proc = System.Diagnostics.Process.GetProcessById(appPid);
        gdi = GetGuiResources(proc.Handle, 0); user = GetGuiResources(proc.Handle, 1);
      } catch {}
    }
    var r = Find(title);
    if (r.IsEmpty) return "{\"found\":false,\"gdi\":" + gdi + ",\"user\":" + user + "}";
    int on = 0, changes = 0, samples = 0, litSamples = 0, foregroundApp = 0, colourChanges = 0;
    double inputAgeAtForeground = -1; bool cursorOnButton = false; double firstForeground = -1, lastChange = -1, top = double.MinValue, bottom = double.MaxValue, lastMean = double.NaN;
    bool lit = false, first = true;
    var clock = System.Diagnostics.Stopwatch.StartNew();
    using (var b = new Bitmap(r.Width, r.Height)) using (var g = Graphics.FromImage(b)) {
      var lastFind = 0.0;
      do {
        // Other buttons coming and going move this one along the taskbar.
        if (clock.Elapsed.TotalSeconds - lastFind > 10) {
          lastFind = clock.Elapsed.TotalSeconds;
          var moved = Find(title);
          if (!moved.IsEmpty) r.Location = moved.Location;
        }
        g.CopyFromScreen(r.Location, Point.Empty, r.Size);
        long sr = 0, sg = 0, sb = 0, n = 0;
        for (int y = 0; y < r.Height; y += 2) for (int x = 0; x < r.Width; x += 2) { var c = b.GetPixel(x, y); sr += c.R; sg += c.G; sb += c.B; n++; }
        double rb = (double)(sr - sb) / n;
        double mean = (double)(sr + sg + sb) / (3 * n);
        top = Math.Max(top, rb); bottom = Math.Min(bottom, rb);
        // The attention colour is a red fill. Idle is grey, so red minus blue tells them apart.
        bool now = rb > 12;
        if (first) { lit = now; if (now) on++; first = false; }
        else if (now != lit) { changes++; lit = now; lastChange = clock.Elapsed.TotalSeconds; if (now) on++; }
        if (!double.IsNaN(lastMean) && Math.Abs(mean - lastMean) > 1.5) colourChanges++;
        lastMean = mean;
        if (now) litSamples++;
        samples++;
        if (appPid != 0 && ForegroundPid() == appPid) { foregroundApp++; if (firstForeground < 0) { firstForeground = clock.Elapsed.TotalSeconds; Point c; GetCursorPos(out c); cursorOnButton = r.Contains(c); inputAgeAtForeground = InputAge(); } }
        Thread.Sleep(40);
      } while (clock.Elapsed.TotalSeconds < seconds);
    }
    return string.Format(System.Globalization.CultureInfo.InvariantCulture,
      "{{\"found\":true,\"width\":{0},\"samples\":{1},\"litSamples\":{2},\"pulses\":{3},\"changes\":{4},\"lastChange\":{5:F1},\"litAtEnd\":{6},\"foregroundApp\":{7},\"firstForeground\":{13:F1},\"cursorOnButton\":{14},\"inputAgeAtForeground\":{15:F1},\"colourChanges\":{8},\"redBlueMin\":{9:F1},\"redBlueMax\":{10:F1},\"gdi\":{11},\"user\":{12}}}",
      r.Width, samples, litSamples, on, changes, lastChange, lit ? "true" : "false", foregroundApp, colourChanges, bottom, top, gdi, user, firstForeground, cursorOnButton ? "true" : "false", inputAgeAtForeground);
  }
}
'@
Add-Type -TypeDefinition $src -ReferencedAssemblies System.Drawing,UIAutomationClient,UIAutomationTypes,WindowsBase
[Watch]::Run($Title, $Seconds, $AppPid)
