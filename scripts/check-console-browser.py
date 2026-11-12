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
    status = {'version': 'browser-fixture', 'credentials': credentials,
              'models': [{'id': 'open-design-deepseek-v4.1-flash', 'upstream_id': 'deepseek-v4.1-flash'},
                         {'id': 'open-design-minimax-m3.1', 'upstream_id': 'minimax-m3.1'}]}
    # Synthetic log rows covering every cell: latency tones, usage details, a
    # failure, a local rejection and a row an older version would have written.
    logs = [
        {'id': 'log-1', 'started_at': stamp(-120), 'finished_at': stamp(-112), 'duration_ms': 8421, 'first_token_ms': 612,
         'model': 'open-design-deepseek-v4.1-flash', 'upstream_model': 'deepseek-v4.1-flash', 'credential_id': 'fixture-01',
         'stream': True, 'status': 'success', 'http_status': 200, 'error_kind': None, 'upstream_attempted': True,
         'input_tokens': 18230, 'output_tokens': 1422, 'total_tokens': 19652, 'cached_tokens': 15104, 'reasoning_tokens': 310,
         'output_tokens_per_second': 182.1, 'reasoning_effort': 'high', 'request_id': 'req-fixture-1'},
        {'id': 'log-2', 'started_at': stamp(-240), 'finished_at': stamp(-188), 'duration_ms': 52800, 'first_token_ms': 11800,
         'model': 'open-design-minimax-m3.1', 'upstream_model': 'minimax-m3.1', 'credential_id': 'fixture-01',
         'stream': True, 'status': 'error', 'http_status': 200, 'error_kind': 'upstream_protocol_error', 'upstream_attempted': True,
         'input_tokens': None, 'output_tokens': None, 'total_tokens': None, 'output_tokens_per_second': None},
        {'id': 'log-3', 'started_at': stamp(-360), 'finished_at': stamp(-360), 'duration_ms': 2,
         'model': 'open-design-deepseek-v4.1-flash', 'upstream_model': 'deepseek-v4.1-flash', 'credential_id': 'fixture-02',
         'stream': False, 'status': 'error', 'http_status': None, 'error_kind': 'quota_blocked', 'upstream_attempted': False,
         'input_tokens': None, 'output_tokens': None, 'total_tokens': None, 'output_tokens_per_second': None},
        {'id': 'log-4', 'started_at': stamp(-480), 'finished_at': stamp(-476), 'duration_ms': 4000,
         'model': 'open-design-minimax-m3.1', 'upstream_model': 'minimax-m3.1', 'credential_id': 'fixture-01',
         'stream': True, 'status': 'success', 'http_status': 200, 'error_kind': None, 'upstream_attempted': True,
         'input_tokens': 100, 'output_tokens': 40, 'total_tokens': 140, 'output_tokens_per_second': 10.0},
    ]
    stats = {'total': 4, 'success': 2, 'failed': 2, 'streams': 3, 'success_rate': 0.5, 'upstream_attempted': 3, 'local_rejected': 1,
             'tokens': {'input': 18330, 'output': 1462, 'total': 19792, 'cached': 15104, 'reasoning': 310,
                        'cache_rate': 0.8285, 'cache_samples': 1, 'requests_with_usage': 2, 'requests_without_usage': 2},
             'output_tokens_per_second': 123.4, 'output_rate_samples': 2, 'first_token_ms': 6206, 'first_token_samples': 2,
             'range': {'since': None, 'until': None}, 'models': [],
             'retention': {'enabled': True, 'retention_days': 7, 'max_entries': 500, 'entries': 4}}
    stats_queries = []
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
        # One account is overdrawn: the upstream reports that as a negative
        # balance, which must be shown as the amount it is.
        balance = '-0.1234' if credential_id == 'fixture-03' else '12.345'
        return {'credential_id': credential_id, 'billing_configured': True, 'workspace_id': 'fixture-workspace',
                'state': 'available', 'fetched_at': stamp(0), 'next_refresh_at': stamp(60),
                'wallet': {'state': 'available', 'fetched_at': stamp(0), 'data': {'balanceUsd': balance, 'updatedAt': stamp(0)}},
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
        elif parsed.path == base + '/request-logs':
            # The console lands on the logs tab and reads it once.
            fulfill(route, {'logs': logs, 'total': len(logs), 'limit': 50, 'offset': 0, 'has_more': False,
                            'next_offset': None, 'retention': stats['retention']})
        elif parsed.path == base + '/request-stats':
            stats_queries.append(parse_qs(parsed.query))
            fulfill(route, stats)
        elif parsed.path == base + '/config':
            fulfill(route, {'logs': {'enabled': True, 'retention_days': 7, 'max_entries': 500, 'entries': 4},
                            'pause': {'enabled': True, 'max_seconds': 86400, 'owner': 'plugin'},
                            'concurrency': {'enabled': True, 'max_in_flight': 4, 'hold_seconds': 60, 'max_hold_seconds': 300,
                                            'rest_seconds': 65, 'owner': 'plugin',
                                            'limits': {'max_in_flight': [1, 64], 'hold_seconds': [0, 3600], 'max_hold_seconds': [0, 3600],
                                                       'rest_seconds': [0, 3600]},
                                            'stats': {'since': stamp(-7200), 'held_now': 1, 'rejected_at_limit': 1234,
                                                      'rejected_while_held': 56, 'rejected_at_rest': 7890,
                                                      'upstream_rate_limited': 9, 'holds_started': 7}},
                            'proxy': {'mode': 'host_managed', 'owner': 'CPA', 'read_only': True},
                            'routing': {'mode': 'host_managed', 'owner': 'CPA', 'read_only': True},
                            'upstream': {'fixed': True, 'origin': 'https://amr-link.open-design.ai/v1'}})
        elif parsed.path == base + '/runtime':
            gate = {'fixture-01': {'in_flight': 3, 'peak_in_flight': 4, 'held_until': None, 'resting_until': stamp(40), 'probing': False, 'consecutive_rejections': 0},
                    'fixture-02': {'in_flight': 0, 'peak_in_flight': 0, 'held_until': None, 'resting_until': None, 'probing': False, 'consecutive_rejections': 0},
                    'fixture-03': {'in_flight': 0, 'peak_in_flight': 4, 'held_until': stamp(45), 'resting_until': None, 'probing': False, 'consecutive_rejections': 2}}
            fulfill(route, {'loaded': True, 'stopped': False, 'active_requests': 3, 'pending_operations': 0, 'model_count': 2,
                            'host': {'available': True}, 'observed_at': stamp(0),
                            'concurrency': {'enabled': True, 'max_in_flight': 4, 'rest_seconds': 65},
                            'credentials': [{'id': c['id'], 'label': c['label'], 'disabled': False, 'model_count': 2,
                                             'models_scope': 'shared', 'host': {'status': 'active'},
                                             'concurrency': gate[c['id']]} for c in credentials[:3]]})
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
        page.wait_for_function('OpenDesignConsole.state.logAutoLoaded === true')
        assert page.locator('#tabLogs').get_attribute('aria-selected') == 'true', 'the console lands on the logs tab'
        assert not page.locator('#authDialog').evaluate('node => node.open'), 'a reused login keeps the key dialog closed'

        # ---- request logs -------------------------------------------------
        page.locator('#requestLogList details').nth(3).wait_for()
        overflow = 'document.documentElement.scrollWidth <= innerWidth'
        rows = page.locator('#requestLogList details')
        assert rows.count() == 4
        first_row = rows.nth(0).locator('summary')
        assert '首字 612 ms' in first_row.inner_text().replace('\n', ' ')
        assert '182 t/s' in first_row.inner_text() and '82.85%' in first_row.inner_text()
        tones = first_row.locator('.latency-line')
        assert tones.count() == 2 and 'good' in tones.nth(0).get_attribute('class')
        assert rows.nth(1).locator('.latency-line.bad').count() == 2, 'an 11.8s first token and a 52.8s reply are both red'
        assert rows.nth(2).locator('.badge').inner_text() == '本地拒绝 · 额度暂停'
        assert '未请求上游' in rows.nth(2).inner_text() and 'HTTP' not in rows.nth(2).locator('summary').inner_text()
        assert '缓存' not in rows.nth(3).inner_text(), 'a row without cache data invents none'
        cards = page.locator('#requestStats .fact.stat')
        assert cards.count() == 5
        widths = [cards.nth(i).bounding_box()['width'] for i in range(5)]
        assert max(widths) - min(widths) < 2 and min(widths) > 200, f'statistics cards must share the row evenly: {widths}'
        badge_box = rows.nth(0).locator('.badge').bounding_box()
        assert badge_box['width'] < 80, f'a status badge must not stretch across its column: {badge_box}'
        assert page.evaluate(overflow), 'logs: desktop horizontal overflow'
        page.screenshot(path=str(args.out / 'logs-desktop.png'))
        page.locator('#statsRange24h').click()
        page.wait_for_function('OpenDesignConsole.state.statsRange === "24h" && !OpenDesignConsole.state.optionalBusy.stats')
        assert stats_queries[-1].get('since'), 'a statistics range sends a since bound'
        assert requests.count(base + '/request-logs') == 1, 'changing the range does not re-read the list'
        assert page.locator('#statsRange24h').get_attribute('aria-pressed') == 'true'
        rows.nth(0).locator('summary').click()
        assert rows.nth(0).locator('.log-detail').is_visible()
        assert 'req-fixture-1' in rows.nth(0).locator('.log-detail').inner_text()

        # ---- models and config ---------------------------------------------
        page.locator('#tabModels').click()
        mapping = page.locator('#mappingList .mapping-row').first
        alias = mapping.locator('input').first
        assert alias.evaluate('node => node.scrollWidth <= node.clientWidth'), 'a default-prefixed alias must fit at 1440px'
        assert alias.get_attribute('title') == 'open-design-deepseek-v4.1-flash', 'the full alias stays available on hover'
        assert mapping.locator('[data-action="test-mapping"]').is_enabled()
        left = page.locator('#modelsPanel').bounding_box()
        right = page.locator('#reasoningPanel').bounding_box()
        assert right['x'] > left['x'] + left['width'] - 1, 'models and reasoning panels sit side by side on desktop'
        assert page.evaluate(overflow), 'models: desktop horizontal overflow'
        page.screenshot(path=str(args.out / 'models-desktop.png'))
        page.locator('#tabConfig').click()
        page.locator('#loadConfig').click()
        page.wait_for_function('OpenDesignConsole.state.config !== null')
        page.locator('#loadRuntime').click()
        page.locator('[data-runtime-credential="fixture-02"]').wait_for()
        assert '额度暂停中' in page.locator('[data-runtime-credential="fixture-02"]').inner_text()
        assert '额度暂停中' not in page.locator('[data-runtime-credential="fixture-01"]').inner_text()
        assert page.locator('#configPauseEnabled').is_checked()
        # ---- concurrency protection: settings, counters, runtime state ----
        assert page.locator('#configConcurrencyEnabled').is_checked()
        assert page.locator('#configConcurrencyRestSeconds').input_value() == '65'
        assert page.locator('#configConcurrencyMaxInFlight').input_value() == '4'
        assert page.locator('#configConcurrencyHoldSeconds').input_value() == '60'
        assert page.locator('#configConcurrencyMaxHoldSeconds').input_value() == '300'
        assert page.locator('#saveConcurrencyConfig').is_enabled()
        gate_facts = page.locator('#concurrencyFacts').inner_text().replace('\n', ' ')
        for expected in ('当前挂起账号 1', '休息期间被本地拒绝 7890', '达到上限被本地拒绝 1234', '挂起期间被本地拒绝 56', '上游限流次数 9', '触发挂起次数 7'):
            assert expected in gate_facts, f'concurrency facts miss {expected!r}: {gate_facts}'
        busy_row = page.locator('[data-runtime-credential="fixture-01"]').inner_text().replace('\n', ' ')
        assert '在途 3 / 上限 4（峰值 4）' in busy_row and '限流挂起中' not in busy_row
        assert '休息中' in busy_row and '休息到' in busy_row, busy_row
        held_row = page.locator('[data-runtime-credential="fixture-03"]').inner_text().replace('\n', ' ')
        assert '限流挂起中' in held_row and '挂起到' in held_row and '休息中' not in held_row
        runtime_facts = page.locator('#runtimeFacts').inner_text().replace('\n', ' ')
        assert '账号并发上限 4' in runtime_facts and '请求完成后休息 65 秒' in runtime_facts, runtime_facts
        panel = page.locator('#concurrencySettingsForm')
        pause_panel = page.locator('#pauseSettingsForm')
        assert panel.bounding_box()['y'] > pause_panel.bounding_box()['y'], 'the concurrency panel follows the quota-pause panel'
        field_widths = [page.locator('#' + name).bounding_box()['width'] for name in
                        ('configConcurrencyRestSeconds', 'configConcurrencyMaxInFlight', 'configConcurrencyHoldSeconds',
                         'configConcurrencyMaxHoldSeconds', 'configPauseMaxSeconds')]
        assert max(field_widths) - min(field_widths) < 2, f'the new fields must line up with the existing ones: {field_widths}'
        # An out-of-range number never reaches the handler: the browser's own
        # constraint validation stops the submit first.
        page.locator('#configConcurrencyMaxInFlight').fill('65')
        assert not page.locator('#configConcurrencyMaxInFlight').evaluate('node => node.validity.valid')
        assert not page.locator('#concurrencySettingsForm').evaluate('node => node.checkValidity()')
        page.locator('#configConcurrencyMaxInFlight').fill('4')
        # A maximum below the hold passes every per-field constraint, so a real
        # click submits the form and only the handler can refuse it. That proves
        # the submit event is bound; the fixture (GET only) proves nothing was sent.
        page.locator('#configConcurrencyHoldSeconds').fill('200')
        page.locator('#configConcurrencyMaxHoldSeconds').fill('100')
        assert page.locator('#concurrencySettingsForm').evaluate('node => node.checkValidity()')
        page.locator('#saveConcurrencyConfig').click()
        page.wait_for_function("document.getElementById('configState').textContent.includes('最长挂起不能小于挂起时间')")
        page.locator('#configConcurrencyHoldSeconds').fill('60')
        page.locator('#configConcurrencyMaxHoldSeconds').fill('300')
        assert page.evaluate(overflow), 'config: desktop horizontal overflow'
        page.screenshot(path=str(args.out / 'config-desktop.png'), full_page=True)

        # ---- dialogs --------------------------------------------------------
        page.locator('#openAuth').click()
        assert page.locator('#authDialog').evaluate('node => node.open')
        assert page.locator('#managementKey').evaluate('node => node === document.activeElement'), 'the key field takes focus'
        page.keyboard.press('Escape')
        assert not page.locator('#authDialog').evaluate('node => node.open')
        assert page.evaluate('Boolean(OpenDesignConsole.state.verified)'), 'dismissing the key dialog keeps the session'
        page.locator('#tabLogs').click()
        assert base + '/quota' not in requests, 'no tab other than credentials queries quota'
        page.locator('#tabCredentials').click()
        page.locator('[data-credential-id="fixture-01"] [data-quota-status="loading"]').wait_for()
        page.locator('[data-credential-id="fixture-02"] [data-quota-status="pending"]').wait_for()
        assert len(held) == 1, 'serial walk must have exactly one in-flight request'
        page.locator('#tabConfig').click()
        hold[0] = False
        for route, cid in held:
            fulfill(route, quota(cid))
        page.wait_for_function('!OpenDesignConsole.state.quotaBusy["fixture-01"]')
        assert requests.count(base + '/quota') == 1, 'tab exit must stop remaining queries'
        page.wait_for_function('!OpenDesignConsole.state.optionalBusy.config')
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
        # An overdrawn wallet is shown as it is, in the warning colour, and it
        # is added to the total like any other balance.
        third = page.locator('[data-credential-id="fixture-03"]')
        assert '-$0.1234' in third.locator('summary.account-summary').inner_text()
        third.locator('summary.account-summary').click()
        negative = third.locator('[data-quota-part="wallet"] .quota-value')
        assert negative.inner_text() == '-$0.1234'
        assert 'negative' in negative.get_attribute('class')
        danger = page.evaluate("getComputedStyle(document.documentElement).getPropertyValue('--danger').trim()")
        probe = page.evaluate('''(colour) => { const n = document.createElement('span'); n.style.color = colour;
            document.body.append(n); const c = getComputedStyle(n).color; n.remove(); return c; }''', danger)
        assert negative.evaluate('node => getComputedStyle(node).color') == probe, 'a negative balance uses the danger colour'
        assert '钱包余额为负' in third.locator('[data-quota-part="wallet"]').inner_text()
        third.locator('summary.account-summary').click()
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
        # The account form is a dialog; Esc cancels it and clears what was typed.
        page.locator('#openAccountDialog').click()
        assert page.locator('#accountDialog').evaluate('node => node.open')
        assert page.locator('#accountLabel').evaluate('node => node === document.activeElement')
        page.locator('#accountKey').fill('typed-then-escaped')
        page.keyboard.press('Escape')
        assert not page.locator('#accountDialog').evaluate('node => node.open')
        assert page.locator('#accountKey').input_value() == '', 'Esc must not leave a typed key in the form'
        toggle = first.locator('[data-action="toggle-account"]')
        assert toggle.get_attribute('role') == 'switch' and toggle.get_attribute('aria-checked') == 'true'
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
        page.evaluate('OpenDesignConsole.render.applyThemePreference("light")')
        for tab, name in [('tabLogs', 'logs'), ('tabModels', 'models'), ('tabConfig', 'config')]:
            page.locator('#' + tab).click()
            assert page.evaluate('document.documentElement.scrollWidth <= innerWidth'), f'{name}: mobile horizontal overflow'
            page.screenshot(path=str(args.out / f'{name}-mobile.png'))
        latency = page.locator('#requestLogList details').nth(0).locator('.log-latency')
        page.locator('#tabLogs').click()
        assert latency.bounding_box()['width'] > 100, 'the latency cell must not collapse into the chevron column'
        assert not errors, errors
        result = {'browser': Path(args.browser).name, 'fixture_credentials': len(credentials),
                  'windows_per_card': 3, 'desktop_card_height': desktop_height,
                  'checks': ['no quota on bootstrap', 'serial pending/loading', 'cancel on tab exit',
                             'fresh-snapshot skip', '0/100 percent', 'real reset timestamps', 'native keyboard disclosure',
                             'repaint and page retention', 'stale failure keeps values', 'desktop/mobile no overflow', 'no JS errors',
                             'logs landing read', 'latency tones and usage details', 'local rejection reason',
                             'statistics cards and range', 'models side by side', 'runtime pause badge',
                             'key dialog and account dialog dismissal',
                             'concurrency settings and counters', 'runtime in-flight, rest and hold badges',
                             'negative wallet balance shown with the danger colour'],
                  'screenshots': str(args.out), 'production_requests': 0}
        (args.out / 'result.json').write_text(json.dumps(result, ensure_ascii=False, indent=2) + '\n')
        print(json.dumps(result, ensure_ascii=False))
        context.close()
        browser.close()


if __name__ == '__main__':
    main()
