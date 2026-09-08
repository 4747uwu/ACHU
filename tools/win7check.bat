@echo off
setlocal enabledelayedexpansion

rem tools\win7check.bat - why does TLS stall on a Windows 7 site machine?
rem
rem     win7check.bat        report only, changes nothing
rem     win7check.bat fix    also (re)import the CA root the app ships
rem
rem Run as Administrator. Read-only unless you pass "fix", and "fix" does
rem nothing more than the installer already does - imports one public CA root.
rem
rem Chases the two things that make a Go TLS handshake TIME OUT on Win7 while
rem the same URL opens fine in a browser:
rem
rem  1. Chain building blocks on the network. Win7's root store predates ISRG
rem     Root X2, so CryptoAPI tries to FETCH the missing issuer over AIA and
rem     check revocation before it will trust the chain. When those fetches
rem     hang, the wait happens INSIDE the handshake and surfaces as a plain
rem     "TLS handshake timeout" -- never as a certificate error, which is why
rem     this hides so well. A browser hides it too: it ships its own roots.
rem
rem  2. Antivirus SSL scanning. K7, Quick Heal, Seqrite, Net Protector and
rem     eScan are common on Indian clinic machines and all intercept TLS.
rem     They install their own root and special-case browsers, so Chrome
rem     works while a non-browser process like tarang-sender.exe stalls.

set "APPDIR=C:\Program Files (x86)\Achyu PACS 2"
if not exist "%APPDIR%" set "APPDIR=C:\Program Files\Achyu PACS 2"

echo.
echo ================= OS =================
ver
echo.

echo ======= ANTIVIRUS (running processes) =======
rem tasklist, deliberately. NOT "wmic product get name": that validates every
rem installed MSI and takes minutes, which is exactly why it looked hung. And
rem not the SecurityCenter2 namespace either - wmic is removed outright on
rem current Windows, so it hangs there instead of erroring.
tasklist /fo csv /nh 2>nul | findstr /i "k7 quickheal qhact seqrite escan avast avgsvc avgui mcafee mfemms norton npav bdagent avp sophos webroot cmdagent"
if errorlevel 1 echo   (no known SSL-scanning AV process running)
echo.
echo   Anything listed above is a prime suspect. Test it by turning its
echo   web / SSL shield off for two minutes and restarting the app. Do not
echo   uninstall anything to test this.
echo.

echo ======= ISRG ROOTS (machine store) =======
certutil -store root 2>nul | findstr /i "ISRG"
if errorlevel 1 echo   *** NONE FOUND - very likely the cause on this OS ***
echo.

echo ======== ISRG ROOTS (user store) =========
certutil -store -user root 2>nul | findstr /i "ISRG"
if errorlevel 1 echo   (none in the user store)
echo.

echo ========= CERT SHIPPED WITH APP =========
if exist "%APPDIR%\certs\isrg-root-x2.der" (
  echo   found: %APPDIR%\certs\isrg-root-x2.der
) else (
  echo   *** MISSING: %APPDIR%\certs\isrg-root-x2.der ***
  echo   The installer's certificate step did not run or did not land.
)
echo.

echo ============== PROXY ================
reg query "HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings" /v ProxyServer 2>nul | findstr /i proxyserver
if errorlevel 1 echo   (no WinINET proxy configured)
echo.

if /i "%~1"=="fix" (
  echo ============ APPLYING FIX ===========
  if exist "%APPDIR%\certs\isrg-root-x2.der" (
    certutil -addstore -f root "%APPDIR%\certs\isrg-root-x2.der"
  ) else (
    echo   cannot import - the .der file is not on this machine.
  )
  echo.
  echo   Restart Achyu PACS 2 and watch the log:
  echo     fixed -^> "peer reachable - resuming uploads"
  echo     same  -^> not the certificate; suspect the AV listed above.
  echo.
) else (
  echo   Report only. To import the CA root the app ships, run:
  echo       win7check.bat fix
  echo.
)
endlocal
