#!/usr/bin/env node
//
// Tests for backend-API host failover: apiFallbackBases in main.js and the
// login-time loginUrlsFor / loginFetch pair in the renderer.
//
//   node electron/main/api-failover.test.js
//
// Neither file can be require()d — main.js calls electron APIs at load and
// login.js touches document — so the pure functions are extracted by source
// range and evaluated on their own, exactly as peer-selection.test.js does.
// The stakes: getting the 4xx rule wrong replays a wrong password against a
// second server, and getting apiFallbackBases wrong points an operator's own
// backend at ours.

const fs = require('fs');
const path = require('path');

const mainSrc  = fs.readFileSync(path.join(__dirname, 'main.js'), 'utf8');
const loginSrc = fs.readFileSync(path.join(__dirname, '..', 'renderer', 'login.js'), 'utf8');

function slice(src, label, startMarker, endMarker) {
  const a = src.indexOf(startMarker);
  const b = src.indexOf(endMarker, a);
  if (a < 0 || b < 0) {
    throw new Error(`could not extract ${startMarker} .. ${endMarker} from ${label}`);
  }
  return src.slice(a, b);
}

// main.js: the API host list and the fallback-base derivation.
const mainCode = slice(mainSrc, 'main.js', 'const API_HOSTS =', 'function peerHost(');
const main = new Function(`${mainCode}
  return { API_HOSTS, apiFallbackBases };
`)();

// login.js: the renderer's own copy of the host list, plus the failover fetch.
const loginCode = slice(loginSrc, 'login.js', 'const API_HOSTS =', 'const K_API_URL');
const login = new Function(`${loginCode}
  return { API_HOSTS, loginUrlsFor, loginFetch };
`)();

const { apiFallbackBases } = main;
const { loginUrlsFor, loginFetch } = login;

let failures = 0;
function check(name, got, want) {
  const g = JSON.stringify(got);
  const w = JSON.stringify(want);
  if (g === w) {
    console.log(`  ok   ${name}`);
  } else {
    console.log(`  FAIL ${name}\n         got  ${g}\n         want ${w}`);
    failures++;
  }
}

// The two copies of the host list must agree, or the engine and the login
// screen fail over to different servers.
console.log('host lists');
check('main and login agree', main.API_HOSTS, login.API_HOSTS);

console.log('apiFallbackBases');
check(
  'pacs -> achyu',
  apiFallbackBases('https://pacs.achyutrs.com/api'),
  ['https://achyu.xcentic.com/api'],
);
// Symmetric: a lab that logged into the fallback must still reach the primary.
check(
  'achyu -> pacs',
  apiFallbackBases('https://achyu.xcentic.com/api'),
  ['https://pacs.achyutrs.com/api'],
);
// An operator's own server is never redirected to ours.
check('foreign base has no fallbacks', apiFallbackBases('https://my.own.server/api'), []);
check('malformed base has no fallbacks', apiFallbackBases('not a url'), []);
check('empty base has no fallbacks', apiFallbackBases(''), []);

console.log('loginUrlsFor');
{
  const urls = loginUrlsFor('https://pacs.achyutrs.com/api/auth/lab-login');
  check('both hosts, preferred first, path kept', urls, [
    'https://pacs.achyutrs.com/api/auth/lab-login',
    'https://achyu.xcentic.com/api/auth/lab-login',
  ]);
}
{
  // Logged into the fallback: that host is tried first.
  const urls = loginUrlsFor('https://achyu.xcentic.com/api/auth/lab-login');
  check('host in the url leads', urls, [
    'https://achyu.xcentic.com/api/auth/lab-login',
    'https://pacs.achyutrs.com/api/auth/lab-login',
  ]);
}
check(
  'custom host stands alone',
  loginUrlsFor('https://my.own.server/api/auth/lab-login'),
  ['https://my.own.server/api/auth/lab-login'],
);

// A stub fetch recording every URL it was asked for; `plan` maps a hostname to
// either a thrown Error or a { status } response.
function stubFetch(plan) {
  const calls = [];
  const fn = async (url) => {
    calls.push(url);
    const outcome = plan[new URL(url).hostname];
    if (outcome instanceof Error) throw outcome;
    return outcome;
  };
  fn.calls = calls;
  return fn;
}

const LOGIN = 'https://pacs.achyutrs.com/api/auth/lab-login';

console.log('loginFetch');
(async () => {
  // Primary host unreachable — the login still goes through on the fallback.
  {
    const f = stubFetch({
      'pacs.achyutrs.com': new Error('ENOTFOUND'),
      'achyu.xcentic.com': { status: 200 },
    });
    const res = await loginFetch(LOGIN, {}, f);
    check('network error falls over', res.status, 200);
    check('network error tried both', f.calls.length, 2);
  }

  // A 502 from a broken front end is an outage, not an answer.
  {
    const f = stubFetch({
      'pacs.achyutrs.com': { status: 502 },
      'achyu.xcentic.com': { status: 200 },
    });
    const res = await loginFetch(LOGIN, {}, f);
    check('500 falls over', res.status, 200);
    check('500 reached the fallback', f.calls[1], 'https://achyu.xcentic.com/api/auth/lab-login');
  }

  // The important one: a rejected credential is returned as-is and the second
  // host never sees the password.
  {
    const f = stubFetch({
      'pacs.achyutrs.com': { status: 401 },
      'achyu.xcentic.com': { status: 200 },
    });
    const res = await loginFetch(LOGIN, {}, f);
    check('401 is returned, not retried', res.status, 401);
    check('401 called exactly once', f.calls.length, 1);
  }

  // Both hosts down: the operator sees a real error, not undefined.
  {
    const f = stubFetch({
      'pacs.achyutrs.com': new Error('first down'),
      'achyu.xcentic.com': new Error('second down'),
    });
    let msg = null;
    try { await loginFetch(LOGIN, {}, f); } catch (err) { msg = err.message; }
    check('both down surfaces the last error', msg, 'second down');
    check('both down tried both', f.calls.length, 2);
  }

  // A 5xx on the LAST host is an answer too — there is nowhere left to ask.
  {
    const f = stubFetch({
      'pacs.achyutrs.com': { status: 503 },
      'achyu.xcentic.com': { status: 503 },
    });
    const res = await loginFetch(LOGIN, {}, f);
    check('last 500 is returned', res.status, 503);
  }

  console.log('');
  if (failures) {
    console.log(`${failures} FAILED`);
    process.exit(1);
  }
  console.log('all passed');
})();
