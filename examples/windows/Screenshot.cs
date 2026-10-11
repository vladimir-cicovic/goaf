// Screenshot.exe: capture primary display to PNG, no console ever
// (compiled as Windows subsystem). Build on the guest with:
//   csc.exe /target:winexe /out:C:\Temp\shot.exe Screenshot.cs
// Run in the interactive session (scheduled task /IT puts it there).
using System;
using System.Drawing;
using System.Drawing.Imaging;
using System.Windows.Forms;

class Shot
{
    static int Main(string[] args)
    {
        string dest = args.Length > 0 ? args[0] : @"C:\Temp\screen.png";
        try
        {
            Rectangle b = Screen.PrimaryScreen.Bounds;
            using (Bitmap bmp = new Bitmap(b.Width, b.Height))
            using (Graphics g = Graphics.FromImage(bmp))
            {
                g.CopyFromScreen(b.Location, System.Drawing.Point.Empty, b.Size);
                bmp.Save(dest, ImageFormat.Png);
            }
            Console.WriteLine("SHOT: " + dest);
            return 0;
        }
        catch (Exception ex)
        {
            Console.WriteLine("ERROR: " + ex.Message);
            return 1;
        }
    }
}
