#!/usr/bin/env python3
"""Optional real-browser console check, using only local synthetic API fixtures.

Requires Python playwright and a Chromium-compatible browser (Edge preferred).
No production credentials, CPA process or upstream request is used.
"""
import argparse
import json
from pathlib import Path
import shutil
from urllib.parse import parse_qs, urlparse
from datetime import datetime, timedelta, timezone


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--browser', default=shutil.which('microsoft-edge') or shutil.which('google-chrome'))
    parser.add_argument('--out', type=Path, default=Path('/tmp/open-design-console-browser'))
    args = parser.parse_args()
    if not args.browser:
        parser.error('use --browser /path/to/edge-or-chrome')
    from playwright.sync_api import sync_playwright

    ui = Path(__file__).resolve().parents[1] / 'internal/opendesign/ui'
    html = (ui / 'console.html').read_text()
    for token, filename in [('AUTH', 'auth.js'), ('CORE', 'core.js'), ('API', 'api.js'), ('CONSOLE', 'console.js')]:
        html = html.replace('/*__OPEN_DESIGN_' + token + '__*/', (ui / filename).read_text())
    args.out.mkdir(parents=True, exist_ok=True)
    origin = 'https://opendesign-console.test'
    base = '/v0/management/open-design'
    now = datetime.now(timezone.utc)
    stamp = lambda seconds: (now + timedelta(seconds=seconds)).isoformat().replace('+00:00', 'Z')
    credentials = []
    for i in range(1, 14):
        paused = i > 1
        item = {'id': f'fixture-{i:02d}', 'label': f'示例账号 {i:02d}', 'disabled': False,
                'billing_configured': True, 'workspace_id': 'fixture-workspace', 'credits_per_usd': '5000',
                'quota_paused': paused, 'effective_disabled': paused}
        if paused:
            item['quota_pause'] = {'reason': 'quota_exhausted', 'resume_at': stamp(-600),
                                   'estimate_at': stamp(4 * 86400), 'model_id': 'example-model',
                                   'awaiting_recheck': True, 'recheck_status': 'unproven'}
        credentials.append(item)
    status = {'version': 'browser-fixture', 'credentials': credentials, 'models': []}
    failed = set()
    held = []
    hold = [True]
    requests = []
    errors = []

    def quota(credential_id):
        windows = []
        for duration, used in [(18000, '200'), (604800, '1000'), (2592000, '0')]:
            windows.append({'policyId': f'fixture-{duration}', 'durationSeconds': duration, 'resetMode': 'fixed',
                            'usedCredits': used, 'limitCredits': '1000', 'remainingCredits': str(1000 - int(used)),
                            'windowStart': stamp(-3600), 'resetsAt': stamp(duration), 'forecast': {'state': 'insufficient_samples'}})
        return {'credential_id': credential_id, 'billing_configured': True, 'workspace_id': 'fixture-workspace',
                'state': 'available', 'fetched_at': stamp(0), 'next_refresh_at': stamp(60),
                'wallet': {'state': 'available', 'fetched_at': stamp(0), 'data': {'balanceUsd': '12.345', 'updatedAt': stamp(0)}},
                'funding': {'state': 'available', 'fetched_at': stamp(0), 'data': {'balanceUsd': '12.345', 'funding': 'coding_plan',
                            'modelCovered': None, 'codingPlan': {'tier': 'pro', 'windows': windows}}}}

    def fulfill(route, payload, code=200):
        route.fulfill(status=code, content_type='application/json', body=json.dumps(payload))

    def handle(route):
        parsed = urlparse(route.request.url)
        if route.request.url == origin + '/v0/resource/plugins/open-design/console':
            route.fulfill(status=200, content_type='text/html; charset=utf-8', body=html)
            return
        assert parsed.netloc == 'opendesign-console.test', 'unexpected external browser request'
        assert route.request.method == 'GET', 'browser fixtures must not mutate anything'
        requests.append(parsed.path)
        if parsed.path == base + '/status':
            fulfill(route, status)
        elif parsed.path == base + '/scheduling':
            fulfill(route, {'host': {'available': True}, 'accounts': [
                {'id': c['id'], 'state': 'quota_paused' if c['quota_paused'] else 'available', 'host': {}}
                for c in credentials]})
        elif parsed.path == base + '/quota':
            cid = parse_qs(parsed.query)['credential_id'][0]
            if hold[0]:
                held.append((route, cid))
            elif cid in failed:
                fulfill(route, {'error': 'fixture upstream temporarily unavailable'}, 502)
            else:
                fulfill(route, quota(cid))
        else:
            fulfill(route, {'error': 'fixture route not found'}, 404)

    with sync_playwright() as p:
        browser = p.chromium.launch(executable_path=args.browser, headless=True,
                                     args=['--no-sandbox', '--disable-dev-shm-usage'])
        context = browser.new_context(viewport={'width': 1440, 'height': 1000}, locale='zh-CN', timezone_id='Asia/Shanghai')
        context.route('**/*', handle)
        context.add_init_script("sessionStorage.setItem('opendesign:management-auth:" + origin + "', JSON.stringify({key:'local-fixture-only',type:'bearer'}))")
        page = context.new_page()
        page.on('pageerror', lambda error: errors.append(str(error)))
        page.goto(origin + '/v0/resource/plugins/open-design/console')
        page.locator('#accountsPanel').wait_for(state='attached')
        page.wait_for_function('OpenDesignConsole.state.status.credentials.length === 13')
        assert base + '/quota' not in requests, 'bootstrap must not query quota'
        page.locator('#tabCredentials').click()
        page.locator('[data-credential-id="fixture-01"] [data-quota-status="loading"]').wait_for()
        page.locator('[data-credential-id="fixture-02"] [data-quota-status="pending"]').wait_for()
        assert len(held) == 1, 'serial walk must have exactly one in-flight request'
        page.locator('#tabOverview').click()
        hold[0] = False
        for route, cid in held:
            fulfill(route, quota(cid))
        page.wait_for_function('!OpenDesignConsole.state.quotaBusy["fixture-01"]')
        assert requests.count(base + '/quota') == 1, 'tab exit must stop remaining queries'
        page.locator('#tabCredentials').click()
        page.wait_for_function('!OpenDesignConsole.state.quotaAutoRunning')
        assert requests.count(base + '/quota') == 13, 'repeat tab entry skips the already-fresh first row'
        first = page.locator('[data-credential-id="fixture-01"]')
        first_summary = first.locator('summary.account-summary')
        assert first.locator('.account-usage [role="progressbar"]').count() == 3
        assert first.locator('.account-usage [aria-valuenow="100"]').count() == 1
        assert first.locator('.account-usage [aria-valuenow="0"]').count() == 1
        assert '剩余 0' in first_summary.inner_text()
        assert '$12.345' in first_summary.inner_text()
        assert first_summary.locator('button').count() == 0
        assert not first.get_attribute('open')
        assert not first.locator('[data-account-actions]').is_visible(), 'closed details must hide actions'
        # A quota response can repaint before the asynchronous native toggle
        # event. Read connected details.open before replacing the list.
        page.evaluate('''() => {
          const card = document.querySelector('[data-credential-id="fixture-01"]');
          card.open = true;
          OpenDesignConsole.render.renderAccounts();
          if (!document.querySelector('[data-credential-id="fixture-01"]').open) throw Error('disclosure race');
          document.querySelector('[data-credential-id="fixture-01"]').open = false;
          OpenDesignConsole.render.renderAccounts();
        }''')
        assert '上游原提示' in page.locator('[data-credential-id="fixture-02"] > summary.account-summary').inner_text()
        desktop_height = first.bounding_box()['height']
        assert desktop_height < 150, f'collapsed desktop card too tall: {desktop_height}'
        assert page.evaluate('document.documentElement.scrollWidth <= innerWidth'), 'desktop horizontal overflow'
        page.locator('#accountsPanel').evaluate('node => scrollTo(0, node.getBoundingClientRect().top + scrollY - 10)')
        page.screenshot(path=str(args.out / 'desktop-light.png'))
        page.evaluate('OpenDesignConsole.render.applyThemePreference("dark")')
        page.screenshot(path=str(args.out / 'desktop-dark.png'))
        page.evaluate('OpenDesignConsole.render.applyThemePreference("light")')

        # Native keyboard disclosure and state retention on a quota repaint.
        first_summary.focus()
        page.keyboard.press('Enter')
        page.wait_for_function('OpenDesignConsole.state.accountExpanded["fixture-01"] === true')
        assert first.locator('[data-account-actions]').is_visible()
        page.evaluate('OpenDesignConsole.render.renderAccounts()')
        assert first.get_attribute('open') is not None
        assert first_summary.evaluate('node => node === document.activeElement'), 'query repaint must preserve keyboard focus'
        page.locator('#accountsNext').click()
        page.locator('#accountsPrev').click()
        assert first.get_attribute('open') is not None, 'paging must preserve disclosure state'
        failed.add('fixture-01')
        hold[0] = True
        held.clear()
        first.locator('[data-action="refresh-quota"]').focus()
        page.keyboard.press('Enter')
        page.locator('[data-credential-id="fixture-01"] [data-quota-status="loading"]').wait_for()
        assert first_summary.evaluate('node => node === document.activeElement'), 'disabled refresh button must return focus to the summary'
        hold[0] = False
        for route, cid in held:
            fulfill(route, {'error': 'fixture upstream temporarily unavailable'}, 502)
        page.locator('[data-credential-id="fixture-01"] [data-quota-status="error"]').wait_for()
        assert '上次数据' in first_summary.inner_text()
        assert '剩余 800' in first_summary.inner_text(), 'failure must preserve old values, never replace them with zeroes'
        page.screenshot(path=str(args.out / 'expanded-stale.png'))
        first_summary.click()
        page.set_viewport_size({'width': 390, 'height': 844})
        page.locator('#accountsPanel').evaluate('node => scrollTo(0, node.getBoundingClientRect().top + scrollY - 10)')
        assert page.evaluate('document.documentElement.scrollWidth <= innerWidth'), 'mobile horizontal overflow'
        assert first.locator('.account-usage').bounding_box()['width'] <= 390
        page.screenshot(path=str(args.out / 'mobile-light.png'))
        # Use the same actual page in a narrow, dark viewport too.
        page.evaluate('OpenDesignConsole.render.applyThemePreference("dark")')
        page.screenshot(path=str(args.out / 'mobile-dark.png'))
        assert not errors, errors
        result = {'browser': Path(args.browser).name, 'fixture_credentials': len(credentials),
                  'windows_per_card': 3, 'desktop_card_height': desktop_height,
                  'checks': ['no quota on bootstrap', 'serial pending/loading', 'cancel on tab exit',
                             'fresh-snapshot skip', '0/100 percent', 'real reset timestamps', 'native keyboard disclosure',
                             'repaint and page retention', 'stale failure keeps values', 'desktop/mobile no overflow', 'no JS errors'],
                  'screenshots': str(args.out), 'production_requests': 0}
        (args.out / 'result.json').write_text(json.dumps(result, ensure_ascii=False, indent=2) + '\n')
        print(json.dumps(result, ensure_ascii=False))
        context.close()
        browser.close()


if __name__ == '__main__':
    main()
