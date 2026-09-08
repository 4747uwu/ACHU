# tools/netcheck.ps1 - why can this machine not reach the cloud?
#
# Run ON THE SITE MACHINE:
#   powershell -ExecutionPolicy Bypass -File netcheck.ps1
#
# Separates the failures that all surface in the app as "Receiver unreachable"
# but need completely different fixes: DNS, TCP, TLS, and path MTU. The engine
# gives a TLS handshake 10s, so this uses the same 10s and reports where the
# time actually went.

$ErrorActionPreference = 'SilentlyContinue'
$targets = @('router.achyutrs.com', 'pacs.achyutrs.com')
$TLS_BUDGET_MS = 10000

function Measure-Step($block) {
  $sw = [Diagnostics.Stopwatch]::StartNew()
  $r = & $block
  $sw.Stop()
  [PSCustomObject]@{ Ms = [int]$sw.ElapsedMilliseconds; Result = $r }
}

Write-Output "=== netcheck $(Get-Date -Format 'yyyy-MM-dd HH:mm:ss') on $env:COMPUTERNAME ==="
Write-Output ""

Write-Output "--- proxy ---"
Write-Output ("  HTTPS_PROXY env : " + $(if ($env:HTTPS_PROXY) { $env:HTTPS_PROXY } else { '(unset)' }))
Write-Output ("  HTTP_PROXY  env : " + $(if ($env:HTTP_PROXY)  { $env:HTTP_PROXY }  else { '(unset)' }))
$ie = Get-ItemProperty 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Internet Settings'
Write-Output ("  WinINET proxy   : " + $(if ($ie.ProxyEnable -eq 1) { $ie.ProxyServer } else { '(direct)' }))
Write-Output ""

Write-Output "--- interface MTU (what this PC THINKS the path is) ---"
Get-NetIPInterface -AddressFamily IPv4 -ConnectionState Connected |
  Sort-Object InterfaceMetric |
  ForEach-Object { Write-Output ("  {0,-32} MTU {1}" -f $_.InterfaceAlias, $_.NlMtu) }
Write-Output ""

# --- path MTU, measured ------------------------------------------------------
# The one that matters. TCP connects with small packets, so a path that drops
# large ones lets the connection open and then strands the TLS handshake, whose
# certificate messages are several KB. Binary search the largest payload that
# survives; add 28 for the IP+ICMP header to get the real MTU.
Write-Output "--- path MTU probe ---"
$probe = $targets[0]
$lo = 1200; $hi = 1472; $best = 0
while ($lo -le $hi) {
  $mid = [int](($lo + $hi) / 2)
  $out = ping.exe -n 1 -f -l $mid -w 2000 $probe 2>&1 | Out-String
  if ($out -match 'Reply from.*bytes=') { $best = $mid; $lo = $mid + 1 }
  elseif ($out -match 'needs to be fragmented') { $hi = $mid - 1 }
  else { Write-Output "  no ICMP reply at $mid bytes - probe inconclusive"; $best = -1; break }
}
if ($best -gt 0) {
  $pmtu = $best + 28
  Write-Output ("  largest payload that survives : {0} bytes" -f $best)
  Write-Output ("  => actual path MTU            : {0}" -f $pmtu)
  if ($pmtu -lt 1500) {
    Write-Output ""
    Write-Output "  ** This path CANNOT carry 1500-byte packets. If the NIC above"
    Write-Output "     still says 1500, large inbound packets are being dropped and"
    Write-Output "     TLS handshakes will stall. Clamp the NIC to match: **"
    Write-Output ""
    Write-Output ('     netsh interface ipv4 set subinterface "<InterfaceAlias>" mtu={0} store=persistent' -f $pmtu)
  }
}
Write-Output ""

foreach ($h in $targets) {
  Write-Output "=== $h ==="

  # The engine resolves via 8.8.8.8/1.1.1.1 first, bypassing the site's
  # nameserver. If those disagree with the site's answer, the engine is
  # dialling an address the site's network never meant it to use.
  $sys = (Resolve-DnsName $h -Type A | Where-Object { $_.IPAddress } |
            Select-Object -Expand IPAddress) -join ', '
  $pub = (Resolve-DnsName $h -Type A -Server 8.8.8.8 | Where-Object { $_.IPAddress } |
            Select-Object -Expand IPAddress) -join ', '
  Write-Output ("  DNS (site)      : " + $(if ($sys) { $sys } else { 'FAILED' }))
  Write-Output ("  DNS (8.8.8.8)   : " + $(if ($pub) { $pub } else { 'FAILED' }))
  if ($sys -and $pub -and $sys -ne $pub) {
    Write-Output "  ** site and public DNS DISAGREE - the engine uses the public answer **"
  }

  $ip = ($pub -split ', ')[0]
  if (-not $ip) { $ip = ($sys -split ', ')[0] }
  if (-not $ip) { Write-Output "  cannot resolve; skipping"; Write-Output ""; continue }

  $tcp = New-Object Net.Sockets.TcpClient
  $t = Measure-Step { try { $tcp.Connect($ip, 443); $true } catch { $false } }
  if (-not $t.Result) {
    Write-Output ("  TCP 443         : UNREACHABLE after {0} ms" -f $t.Ms)
    Write-Output "                    -> outbound 443 blocked, or no route."
    $tcp.Close(); Write-Output ""; continue
  }
  Write-Output ("  TCP 443         : connected in {0} ms" -f $t.Ms)

  $stream = $tcp.GetStream()
  $stream.ReadTimeout  = $TLS_BUDGET_MS
  $stream.WriteTimeout = $TLS_BUDGET_MS
  # Accept any certificate: the question here is whether the handshake
  # COMPLETES, not whether it validates. The issuer is inspected afterwards
  # via $ssl.RemoteCertificate, which is populated reliably; capturing it from
  # inside the validation callback returned an empty Issuer.
  $cb = [Net.Security.RemoteCertificateValidationCallback] { return $true }
  $ssl = New-Object Net.Security.SslStream($stream, $false, $cb)
  $t = Measure-Step {
    try { $ssl.AuthenticateAsClient($h, $null, [Net.SecurityProtocolType]::Tls12, $false); '' }
    catch { $_.Exception.GetBaseException().Message }
  }
  if ($t.Result -ne '') {
    Write-Output ("  TLS handshake   : FAILED after {0} ms" -f $t.Ms)
    Write-Output ("                    {0}" -f $t.Result)
    if ($t.Ms -ge ($TLS_BUDGET_MS - 500)) {
      Write-Output "                    -> STALLED, not refused. TCP works but the"
      Write-Output "                       handshake got no answer. This is what a"
      Write-Output "                       path-MTU black hole looks like; see probe above."
    }
  } else {
    Write-Output ("  TLS handshake   : OK in {0} ms ({1})" -f $t.Ms, $ssl.SslProtocol)
    $rc = $ssl.RemoteCertificate
    if ($rc) {
      $issuer = ([Security.Cryptography.X509Certificates.X509Certificate2]$rc).Issuer
      Write-Output ("  cert issuer     : " + $issuer)
      # Only meaningful when we actually read an issuer. Silence beats a false
      # accusation: an empty string is missing data, not evidence.
      if ($issuer -and $issuer -notmatch "Let's Encrypt|DigiCert|Sectigo|GlobalSign|Amazon|Google|ISRG|Cloudflare|GoDaddy|Entrust") {
        Write-Output "  ** issuer is not a known public CA - traffic may be INTERCEPTED **"
      }
    }
  }
  $ssl.Close(); $tcp.Close()
  Write-Output ""
}
Write-Output "=== end ==="
