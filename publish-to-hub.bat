@echo off
:: Builds this router and uploads it to Router Hub as client "Achyu".
::
::   publish-to-hub.bat                        build, then upload
::   publish-to-hub.bat -Notes "what changed"  same, with release notes
::   publish-to-hub.bat -SkipBuild             upload the installer already in dist
::
:: Runs the same build steps as build.bat (which is not changed or called) and deletes nothing.
:: The hub address and token live in D:\work\InfraStructure\tools\publish.config.json.
:: The client is matched by ID (6ad06d988c4bd9e737974503), so renaming it in the hub is safe.

powershell -NoProfile -ExecutionPolicy Bypass -File "D:\work\InfraStructure\tools\publish-router.ps1" "%~dp0." -Client "6ad06d988c4bd9e737974503" %*
set PUBLISH_EXIT=%ERRORLEVEL%
pause
exit /b %PUBLISH_EXIT%
