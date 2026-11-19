import { test } from 'node:test';
import assert from 'node:assert/strict';
import { extractJsonBlock } from './jsonblock.js';
import { SceneError } from '../scene/error.js';

test('a fenced json block is extracted without its fences', () => {
  assert.equal(extractJsonBlock('```json\n{"bodies": []}\n```\n'), '{"bodies": []}\n');
});

test('prose around the block is ignored', () => {
  const reply = 'Here is your ramp:\n\n```json\n{"a": 1}\n```\n\nIt is 30 degrees.';
  assert.equal(extractJsonBlock(reply), '{"a": 1}\n');
});

test('a bare fence counts; a fence of another language does not', () => {
  assert.equal(extractJsonBlock('```\n{"a": 1}\n```'), '{"a": 1}\n');
  assert.throws(() => extractJsonBlock('```js\nconst a = 1;\n```'), /no ```json block/);
});

test('with two blocks the last one wins: it is the newest attempt', () => {
  const reply = '```json\n{"draft": 1}\n```\nActually:\n```json\n{"final": 2}\n```';
  assert.equal(extractJsonBlock(reply), '{"final": 2}\n');
});

test('a reply that is only a JSON object is accepted', () => {
  assert.equal(extractJsonBlock('  {"a": 1}  \n'), '{"a": 1}\n');
});

test('an unterminated block is an error, never a partial parse — even after a complete one', () => {
  assert.throws(() => extractJsonBlock('```json\n{"bodies": ['), /unclosed ```json block/);
  assert.throws(() => extractJsonBlock('```json\n{"a": 1}\n```\n```json\n{"b"'), /cut off/);
});

test('failures are SceneErrors, so they travel the same verdict path as invalid scenes', () => {
  try {
    extractJsonBlock('I cannot help with that.');
    assert.fail('expected a throw');
  } catch (error) {
    assert.ok(error instanceof SceneError);
    assert.equal(error.kind, 'syntax');
    assert.deepEqual(error.issues.map((i) => i.path), ['']);
  }
});

test('an empty block is reported as such', () => {
  assert.throws(() => extractJsonBlock('```json\n\n```'), /block is empty/);
});
