# Superseded x64 GDCM 3.2.6 binaries — reference only, NOT shipped

These are the 64-bit **GDCM 3.2.6** binaries the installer used to bundle. The
shipping bundle is now **GDCM 3.2.7 x86**. They are kept here only so the
previous bundle can be recovered; nothing in the build references this
directory.

The version change matters as much as the architecture one: if the DX studies a
site saw rejected by Orthanc (HTTP 400, empty ReferencedSOPSequence) were the
product of a 3.2.6 encoder bug, 3.2.6 is no longer in play. That is now the
first suspect to rule out on the next field test — see
config.DefaultLosslessModalityPolicy.

They were replaced because the product is 32-bit end to end — the Electron
installer is `ia32` and the sender is built `GOARCH=386` — so on a 32-bit
Windows box the x64 `gdcmconv.exe` could not be loaded at all. It was present
on disk, which is all the old availability check tested, so compression
appeared enabled and then failed on every series.

The shipping bundle in the repo root is now GDCM 3.2.7 x86, which runs natively
on 32-bit Windows and through WOW64 on 64-bit Windows. `transcode.Available`
additionally launches the binary before trusting it, so any future mismatch
degrades to "send as-is" instead of failing studies.
