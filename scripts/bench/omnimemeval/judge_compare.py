#!/usr/bin/env python3
"""Calibrate another judge on frozen official answers; never regenerate answers.

OpenAI/dotenv are optional runtime dependencies. Unit checks need stdlib only.
The result measures agreement with a reference judge, not human correctness.
"""
import argparse
import ast
import asyncio
import hashlib
import json
import os
from pathlib import Path
import re
import time
from urllib.parse import urlsplit

PROMPTS_SHA256 = '0e0eadc6a476a89f8d5f8b60e37a0139585b998e228152e66a95109bedb71bbe'
REVISION = '7c79d46e31dd85f3da2509b430bba9406656dd28'


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def read_prompts(path):
    raw = Path(path).read_bytes()
    if sha(raw) != PROMPTS_SHA256:
        raise ValueError('judge prompt source differs from pinned upstream')
    result = {}
    for node in ast.parse(raw).body:
        if isinstance(node, ast.Assign):
            for target in node.targets:
                if isinstance(target, ast.Name) and target.id in ('JUDGE_PROMPT', 'JUDGE_SYSTEM_PROMPT'):
                    result[target.id] = ast.literal_eval(node.value)
    if len(result) != 2 or not all(isinstance(v, str) for v in result.values()):
        raise ValueError('missing literal judge prompts')
    return result


def parse_label(content):
    # Same label extraction shape as upstream, with malformed values recorded
    # as errors rather than silently counted WRONG or retried for a better label.
    match = re.search(r'\{\s*"label"\s*:\s*["\']([^"\']*)["\']\s*\}', content)
    if not match:
        raise ValueError('judge returned no valid CORRECT/WRONG label')
    label = json.loads(match[0])['label'].strip().upper()
    if label not in ('CORRECT', 'WRONG'):
        raise ValueError('judge returned no valid CORRECT/WRONG label')
    return label == 'CORRECT'


def summarize(records, expected):
    valid = [r for r in records if r['status'] == 'success']
    n = len(valid)
    agreement = sum(r['reference_correct'] == r['candidate_correct'] for r in valid)
    disagreements = [r['user_id'] for r in valid if r['reference_correct'] != r['candidate_correct']]
    ref_correct = sum(r['reference_correct'] for r in records)
    return {
        'status': 'complete' if len(records) == expected and n == expected else 'incomplete',
        'expected': expected, 'attempted': len(records), 'paired_valid': n,
        'errors': len(records) - n, 'agreement': agreement,
        'agreement_rate_on_valid_pairs': agreement / n if n else None,
        'reference_correct_all': ref_correct,
        'reference_correct_paired': sum(r['reference_correct'] for r in valid),
        'candidate_correct_paired': sum(r['candidate_correct'] for r in valid),
        'reference_wrong_candidate_correct': sum(not r['reference_correct'] and r['candidate_correct'] for r in valid),
        'reference_correct_candidate_wrong': sum(r['reference_correct'] and not r['candidate_correct'] for r in valid),
        'disagreement_user_ids': disagreements,
        'limitations': ['Judge agreement is not human-verified accuracy.',
                        'Candidate request parameters differ if the model does not support upstream temperature=0.',
                        'No answer regeneration, label-based retries, or change to the official score.'],
    }


async def run(args):
    from openai import AsyncOpenAI
    config = dict(os.environ)
    if args.env_file:
        from dotenv import dotenv_values
        values = dotenv_values(args.env_file)
        for key in (args.api_key_env, args.base_url_env):
            if values.get(key):
                config[key] = values[key]
    key = config.get(args.api_key_env)
    if not key:
        raise ValueError('missing selected API key variable')
    raw = Path(args.judged).read_bytes()
    source = json.loads(raw)
    if len(source) != args.expected_count:
        raise ValueError('reference count differs from expected count')
    indices = [int(uid.rsplit('_', 1)[1]) for uid in source]
    if set(indices) != set(range(args.expected_count)):
        raise ValueError('reference coverage differs from expected indices')
    for row in source.values():
        if row['status'] != 'success' or type(row['llm_judgments']['judgment_1']) is not bool:
            raise ValueError('reference contains unsuccessful or unjudged answers')
    prompts = read_prompts(Path(args.harness) / 'scripts/utils/prompts.py')
    output = Path(args.out)
    output.mkdir(mode=0o700, parents=True, exist_ok=False)
    (output / 'input.json').write_bytes(raw)
    runner = Path(__file__).read_bytes()
    (output / 'runner.py').write_bytes(runner)
    params = {} if args.temperature is None else {'temperature': args.temperature}
    endpoint = config.get(args.base_url_env) or 'https://api.openai.com/v1'
    endpoint_parts = urlsplit(endpoint)
    manifest = {
        'started_at': time.time(), 'input_sha256': sha(raw),
        'prompts_sha256': PROMPTS_SHA256, 'harness_revision': REVISION,
        'candidate_model': args.model, 'request_parameters': params,
        'reasoning': 'provider default; no override', 'expected': args.expected_count,
        'workers': args.workers, 'timeout_seconds': 120, 'sdk_network_retries': 2,
        'runner_sha256': sha(runner),
        'endpoint_origin': endpoint_parts.scheme + '://' + (endpoint_parts.hostname or ''),
        'endpoint_url_sha256': sha(endpoint.encode()),
    }
    (output / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
    semaphore = asyncio.Semaphore(args.workers)
    client = AsyncOpenAI(api_key=key, base_url=endpoint, timeout=120, max_retries=2)

    async def grade(uid, row):
        messages = [
            {'role': 'system', 'content': prompts['JUDGE_SYSTEM_PROMPT']},
            {'role': 'user', 'content': prompts['JUDGE_PROMPT'].format(
                question=row['question'], golden_answer=row['golden_answer'], response=row['answer'])},
        ]
        receipt = {'user_id': uid, 'category': row['category'],
                   'reference_correct': row['llm_judgments']['judgment_1'],
                   'model_input_sha256': sha(json.dumps(messages, ensure_ascii=False).encode()),
                   'status': 'error'}
        async with semaphore:
            at = time.monotonic()
            try:
                response = await client.chat.completions.create(model=args.model, messages=messages, **params)
                content = response.choices[0].message.content or ''
                receipt.update(resolved_model=response.model, raw_content=content,
                               usage=response.usage.model_dump() if response.usage else None)
                receipt.update(candidate_correct=parse_label(content), status='success')
            except Exception as exc:
                receipt['error'] = str(exc).replace(key, '[REDACTED]')
            receipt['duration_ms'] = 1000 * (time.monotonic() - at)
            with (output / 'receipts.jsonl').open('a') as f:
                f.write(json.dumps(receipt, ensure_ascii=False) + '\n')
        return receipt

    try:
        records = await asyncio.gather(*(grade(uid, source[uid]) for uid in sorted(source, key=lambda u: int(u.rsplit('_', 1)[1]))))
    finally:
        await client.close()
    summary = summarize(records, args.expected_count)
    (output / 'summary.json').write_text(json.dumps(summary, indent=2) + '\n')
    print(json.dumps({k: v for k, v in summary.items() if k != 'disagreement_user_ids'}))
    return 0 if summary['status'] == 'complete' else 1


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    for flag in ('judged', 'harness', 'out', 'model'):
        parser.add_argument('--' + flag, required=True)
    parser.add_argument('--env-file')
    parser.add_argument('--api-key-env', default='OPENAI_API_KEY')
    parser.add_argument('--base-url-env', default='OPENAI_BASE_URL')
    parser.add_argument('--temperature', type=float, help='omitted by default; record any protocol difference')
    parser.add_argument('--expected-count', type=int, default=500)
    parser.add_argument('--workers', type=int, default=4)
    options = parser.parse_args()
    if not 1 <= options.workers <= 32 or options.expected_count < 1:
        parser.error('workers must be 1..32 and count must be positive')
    raise SystemExit(asyncio.run(run(options)))
