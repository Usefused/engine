import assert from 'node:assert/strict';
import test from 'node:test';
import { SDK_LANGUAGES, isSDKLanguage, sdkLanguageOrDefault } from './sdk-languages.ts';

// Persisted Go families must keep their compiler when users create a successor.
test('all supported SDK languages round-trip through creation and successor defaults', () => {
  for (const language of SDK_LANGUAGES) {
    assert.equal(isSDKLanguage(language), true);
    assert.equal(sdkLanguageOrDefault(language), language);
  }
  assert.equal(isSDKLanguage('rust'), false);
  assert.equal(isSDKLanguage(undefined), false);
  assert.equal(sdkLanguageOrDefault(undefined), 'typescript');
});
