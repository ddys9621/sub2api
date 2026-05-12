<!--
  KiroAccountWizard.vue

  Standalone modal that walks an operator through creating a Kiro account.
  Kept independent of CreateAccountModal so we do not have to port Kiro's
  idiosyncratic OAuth+CSRF flow into the large shared modal. The wizard
  supports two modes:

  - "oauth"  -> backend generates a Cognito authorize URL, user completes
                Google login in a new tab, pastes the resulting callback URL
                back into the textarea, backend redeems the code and
                persists a new Account row.
  - "manual" -> operator pastes access_token + refresh_token + profile_arn
                directly (eg. captured from Kiro IDE or kiro-cli). Backend
                skips the CBOR ExchangeToken RPC and stores the values
                verbatim.
-->

<template>
  <!-- Embedded mode: render only inner form content, no outer modal shell -->
  <div v-if="embedded" class="space-y-4">
    <div class="flex rounded-lg bg-gray-100 p-1 dark:bg-dark-600">
      <button
        type="button"
        :class="[
          'flex-1 rounded-md px-3 py-2 text-xs font-medium transition sm:text-sm',
          mode === 'builderid'
            ? 'bg-white text-indigo-600 shadow-sm dark:bg-dark-500 dark:text-indigo-400'
            : 'text-gray-600 hover:text-gray-900 dark:text-gray-400'
        ]"
        @click="mode = 'builderid'"
      >
        {{ t('admin.kiro.modeBuilderID', 'AWS Builder ID') }}
      </button>
      <button
        type="button"
        :class="[
          'flex-1 rounded-md px-3 py-2 text-xs font-medium transition sm:text-sm',
          mode === 'oauth'
            ? 'bg-white text-indigo-600 shadow-sm dark:bg-dark-500 dark:text-indigo-400'
            : 'text-gray-600 hover:text-gray-900 dark:text-gray-400'
        ]"
        @click="mode = 'oauth'"
      >
        {{ t('admin.kiro.modeOauth', 'Google OAuth') }}
      </button>
      <button
        type="button"
        :class="[
          'flex-1 rounded-md px-3 py-2 text-xs font-medium transition sm:text-sm',
          mode === 'manual'
            ? 'bg-white text-indigo-600 shadow-sm dark:bg-dark-500 dark:text-indigo-400'
            : 'text-gray-600 hover:text-gray-900 dark:text-gray-400'
        ]"
        @click="mode = 'manual'"
      >
        {{ t('admin.kiro.modeManual', 'Paste Tokens') }}
      </button>
    </div>

    <!-- Builder ID mode (OIDC Device Grant) -->
    <div v-if="mode === 'builderid'" class="space-y-4">
      <div v-if="!builderIDUserCode">
        <p class="mb-2 text-sm text-gray-600 dark:text-gray-300">
          {{ t('admin.kiro.builderIDIntro', 'Login with AWS Builder ID (supports Google / GitHub / Email). Click the button below to start, then complete login in the browser popup.') }}
        </p>
        <button
          type="button"
          class="btn btn-primary w-full"
          :disabled="loading"
          @click="startBuilderIDLogin"
        >
          <Icon v-if="loading" name="refresh" size="sm" class="animate-spin" />
          {{ t('admin.kiro.startBuilderID', 'Start Builder ID Login') }}
        </button>
      </div>

      <div v-else class="space-y-4">
        <!-- User code + verification link -->
        <div class="rounded-lg border border-indigo-200 bg-indigo-50 p-4 dark:border-indigo-700 dark:bg-indigo-900/20">
          <p class="mb-2 text-sm font-medium text-indigo-900 dark:text-indigo-200">
            {{ t('admin.kiro.builderIDStep1', 'Please complete login in the browser:') }}
          </p>
          <div class="mb-3 flex items-center justify-center">
            <span class="rounded-lg bg-white px-6 py-3 font-mono text-2xl font-bold tracking-widest text-indigo-700 shadow-sm dark:bg-dark-800 dark:text-indigo-300">
              {{ builderIDUserCode }}
            </span>
          </div>
          <a
            :href="builderIDVerificationURI"
            target="_blank"
            rel="noopener"
            class="btn btn-primary w-full"
          >
            <Icon name="externalLink" size="sm" />
            {{ t('admin.kiro.openBuilderIDAuth', '打开授权页面') }}
          </a>
          <p class="mt-2 text-center text-xs text-gray-500 dark:text-gray-400">
            {{ t('admin.kiro.builderIDStep2', 'After logging in on the authorization page, this will complete automatically.') }}
          </p>
        </div>

        <!-- Polling status -->
        <div class="flex items-center justify-center gap-2 text-sm text-gray-600 dark:text-gray-400">
          <Icon name="refresh" size="sm" class="animate-spin" />
          <span>{{ t('admin.kiro.builderIDPolling', '等待授权完成...') }}</span>
        </div>
      </div>
    </div>

    <!-- OAuth mode -->
    <div v-if="mode === 'oauth'" class="space-y-4">
      <div v-if="!authURL">
        <p class="mb-2 text-sm text-gray-600 dark:text-gray-300">
          {{ t('admin.kiro.oauthStep1Intro', 'Click the button below to generate a Cognito authorize URL. The server will create a PKCE + state pair.') }}
        </p>
        <button
          type="button"
          class="btn btn-primary w-full"
          :disabled="loading"
          @click="generateAuthURL"
        >
          <Icon v-if="loading" name="refresh" size="sm" class="animate-spin" />
          {{ t('admin.kiro.generateAuthUrl', 'Generate Auth URL') }}
        </button>
      </div>

      <div v-else class="space-y-4">
        <div
          class="rounded-lg border border-indigo-200 bg-indigo-50 p-4 dark:border-indigo-700 dark:bg-indigo-900/20"
        >
          <p class="mb-2 text-sm font-medium text-indigo-900 dark:text-indigo-200">
            {{ t('admin.kiro.oauthStep2Intro', 'Step 1. Open the URL in a new tab and complete Google login.') }}
          </p>
          <div class="mb-2 break-all rounded bg-white p-2 font-mono text-xs text-gray-700 dark:bg-dark-800 dark:text-gray-300">
            {{ authURL }}
          </div>
          <div class="flex gap-2">
            <button type="button" class="btn btn-secondary btn-sm flex-1" @click="copyAuthURL">
              <Icon name="copy" size="sm" />
              {{ copied ? t('common.copied', 'Copied') : t('common.copy', 'Copy') }}
            </button>
            <a
              :href="authURL"
              target="_blank"
              rel="noopener"
              class="btn btn-primary btn-sm flex-1"
            >
              <Icon name="externalLink" size="sm" />
              {{ t('admin.kiro.openInBrowser', 'Open in New Tab') }}
            </a>
          </div>
        </div>

        <div>
          <label class="input-label">
            {{ t('admin.kiro.oauthStep3Intro', 'Step 2. Paste the full callback URL from the browser address bar.') }}
          </label>
          <textarea
            v-model="callbackURL"
            rows="3"
            class="input font-mono text-xs"
            placeholder="https://app.kiro.dev/signin/oauth?code=...&state=..."
          ></textarea>
          <p class="input-hint">
            {{ t('admin.kiro.callbackHint', 'If the page redirects too fast, open F12 Network, tick Preserve log, then copy the signin/oauth request URL.') }}
          </p>

        </div>
      </div>

      <div v-if="!embedded" class="grid gap-3 border-t border-gray-200 pt-4 dark:border-dark-600 sm:grid-cols-2">
        <div>
          <label class="input-label">{{ t('admin.accounts.name', 'Account Name') }}</label>
          <input v-model="name" type="text" class="input" placeholder="Kiro OAuth Account" />
        </div>
        <div>
          <label class="input-label">{{ t('admin.accounts.concurrency', 'Concurrency') }}</label>
          <input v-model.number="concurrency" type="number" min="1" class="input" />
        </div>
      </div>

      <div class="flex gap-2">
        <button type="button" class="btn btn-secondary flex-1" @click="handleClose">
          {{ t('common.cancel', 'Cancel') }}
        </button>
        <button
          type="button"
          class="btn btn-primary flex-1"
          :disabled="loading || !callbackURL.trim() || !sessionID"
          @click="submitOAuth"
        >
          <Icon v-if="loading" name="refresh" size="sm" class="animate-spin" />
          {{ t('admin.kiro.submitOauth', 'Exchange & Create Account') }}
        </button>
      </div>
    </div>

    <!-- Manual mode -->
    <div v-if="mode === 'manual'" class="space-y-4">
      <div>
        <label class="input-label">{{ t('admin.kiro.accountJson', 'Account JSON') }}</label>
        <textarea
          v-model="manualJSON"
          rows="5"
          class="input font-mono text-xs"
          placeholder='{"accessToken":"aoa...","refreshToken":"aor...","provider":"BuilderId","clientId":"...","clientSecret":"..."}'
          @blur="applyManualJSON(false)"
        ></textarea>
        <div class="mt-2 flex items-center gap-2">
          <button type="button" class="btn btn-secondary btn-sm" :disabled="!manualJSON.trim()" @click="applyManualJSON(true)">
            {{ t('admin.kiro.parseJson', 'Parse JSON') }}
          </button>
          <span v-if="manualParsedSummary" class="text-xs text-green-600 dark:text-green-400">{{ manualParsedSummary }}</span>
        </div>
      </div>
      <div>
        <label class="input-label">Access Token <span class="text-red-500">*</span></label>
        <textarea
          v-model="manualAccessToken"
          rows="3"
          class="input font-mono text-xs"
          placeholder="aoaAAAAAGn..."
          @blur="applyAccessTokenJSON(false)"
        ></textarea>
      </div>
      <div>
        <label class="input-label">Refresh Token</label>
        <textarea
          v-model="manualRefreshToken"
          rows="3"
          class="input font-mono text-xs"
          placeholder="aorAAAAAGp..."
        ></textarea>
      </div>
      <div>
        <label class="input-label">Profile ARN</label>
        <input
          v-model="manualProfileArn"
          type="text"
          class="input font-mono text-xs"
          placeholder="arn:aws:codewhisperer:us-east-1:xxx:profile/xxx"
        />
      </div>
      <div>
        <label class="input-label">
          {{ t('admin.accounts.kiro.machineIdLabel', 'Machine ID (设备指纹)') }}
        </label>
        <input
          v-model="manualMachineId"
          type="text"
          class="input font-mono text-xs"
          placeholder="64-char hex, e.g. f3a1b2...   (from Kiro-account-manager export)"
        />
        <p class="input-hint text-xs text-amber-600 dark:text-amber-400">
          {{ t('admin.accounts.kiro.machineIdHint', '从 Kiro-account-manager 的账号 JSON 中复制 machineId 字段，留空则用账号 ID 派生 sha256 兜底（不推荐，AWS 易识别）。') }}
        </p>
      </div>
      <div class="grid gap-3 sm:grid-cols-2">
        <div>
          <label class="input-label">Email</label>
          <input v-model="manualEmail" type="email" class="input" placeholder="user@gmail.com" />
        </div>
        <div>
          <label class="input-label">CSRF Token</label>
          <input v-model="manualCSRF" type="text" class="input font-mono text-xs" />
        </div>
      </div>
      <div v-if="!embedded" class="grid gap-3 sm:grid-cols-2">
        <div>
          <label class="input-label">{{ t('admin.accounts.name', 'Account Name') }}</label>
          <input v-model="name" type="text" class="input" />
        </div>
        <div>
          <label class="input-label">{{ t('admin.accounts.concurrency', 'Concurrency') }}</label>
          <input v-model.number="concurrency" type="number" min="1" class="input" />
        </div>
      </div>

      <div class="flex gap-2">
        <button type="button" class="btn btn-secondary flex-1" @click="handleClose">
          {{ t('common.cancel', 'Cancel') }}
        </button>
        <button
          type="button"
          class="btn btn-primary flex-1"
          :disabled="loading || !manualCanSubmit"
          @click="submitManual"
        >
          <Icon v-if="loading" name="refresh" size="sm" class="animate-spin" />
          {{ t('admin.kiro.submitManual', 'Create Account') }}
        </button>
      </div>
    </div>

    <div
      v-if="errorMessage"
      class="rounded-md border border-red-200 bg-red-50 p-3 text-sm text-red-700 dark:border-red-800 dark:bg-red-900/30 dark:text-red-300"
    >
      {{ errorMessage }}
    </div>
  </div>

  <!-- 独立 Modal 模式：保留原先的完整 overlay+panel（保持向后兼容） -->
  <div v-else-if="modelValue" class="fixed inset-0 z-50 overflow-y-auto">
    <div class="fixed inset-0 bg-black/50" @click="handleClose" />

    <div class="flex min-h-full items-center justify-center p-4">
      <div
        class="relative w-full max-w-2xl rounded-lg bg-white p-6 shadow-xl dark:bg-dark-700"
      >
        <div class="mb-4 flex items-center justify-between">
          <h3 class="text-lg font-semibold text-gray-900 dark:text-white">
            {{ t('admin.kiro.addAccount', 'Add Kiro Account') }}
          </h3>
          <button
            type="button"
            class="rounded-md p-1 text-gray-400 hover:bg-gray-100 hover:text-gray-600 dark:hover:bg-dark-600"
            @click="handleClose"
          >
            <Icon name="x" size="sm" />
          </button>
        </div>

        <div class="mb-4 flex rounded-lg bg-gray-100 p-1 dark:bg-dark-600">
          <button
            type="button"
            :class="[
              'flex-1 rounded-md px-4 py-2 text-sm font-medium transition',
              mode === 'builderid'
                ? 'bg-white text-indigo-600 shadow-sm dark:bg-dark-500 dark:text-indigo-400'
                : 'text-gray-600 hover:text-gray-900 dark:text-gray-400'
            ]"
            @click="mode = 'builderid'"
          >
            {{ t('admin.kiro.modeBuilderID', 'AWS Builder ID') }}
          </button>
          <button
            type="button"
            :class="[
              'flex-1 rounded-md px-4 py-2 text-sm font-medium transition',
              mode === 'oauth'
                ? 'bg-white text-indigo-600 shadow-sm dark:bg-dark-500 dark:text-indigo-400'
                : 'text-gray-600 hover:text-gray-900 dark:text-gray-400'
            ]"
            @click="mode = 'oauth'"
          >
            {{ t('admin.kiro.modeOauth', 'Google OAuth (recommended)') }}
          </button>
          <button
            type="button"
            :class="[
              'flex-1 rounded-md px-4 py-2 text-sm font-medium transition',
              mode === 'manual'
                ? 'bg-white text-indigo-600 shadow-sm dark:bg-dark-500 dark:text-indigo-400'
                : 'text-gray-600 hover:text-gray-900 dark:text-gray-400'
            ]"
            @click="mode = 'manual'"
          >
            {{ t('admin.kiro.modeManual', 'Paste Tokens') }}
          </button>
        </div>

        <!-- Builder ID mode (OIDC Device Grant) -->
        <div v-if="mode === 'builderid'" class="space-y-4">
          <div v-if="!builderIDUserCode">
            <p class="mb-2 text-sm text-gray-600 dark:text-gray-300">
              {{ t('admin.kiro.builderIDIntro', 'Login with AWS Builder ID (supports Google / GitHub / Email). Click the button below to start, then complete login in the browser popup.') }}
            </p>
            <button
              type="button"
              class="btn btn-primary w-full"
              :disabled="loading"
              @click="startBuilderIDLogin"
            >
              <Icon v-if="loading" name="refresh" size="sm" class="animate-spin" />
              {{ t('admin.kiro.startBuilderID', 'Start Builder ID Login') }}
            </button>
          </div>

          <div v-else class="space-y-4">
            <div class="rounded-lg border border-indigo-200 bg-indigo-50 p-4 dark:border-indigo-700 dark:bg-indigo-900/20">
              <p class="mb-2 text-sm font-medium text-indigo-900 dark:text-indigo-200">
                {{ t('admin.kiro.builderIDStep1', 'Please complete login in the browser:') }}
              </p>
              <div class="mb-3 flex items-center justify-center">
                <span class="rounded-lg bg-white px-6 py-3 font-mono text-2xl font-bold tracking-widest text-indigo-700 shadow-sm dark:bg-dark-800 dark:text-indigo-300">
                  {{ builderIDUserCode }}
                </span>
              </div>
              <a
                :href="builderIDVerificationURI"
                target="_blank"
                rel="noopener"
                class="btn btn-primary w-full"
              >
                <Icon name="externalLink" size="sm" />
                {{ t('admin.kiro.openBuilderIDAuth', '打开授权页面') }}
              </a>
              <p class="mt-2 text-center text-xs text-gray-500 dark:text-gray-400">
                {{ t('admin.kiro.builderIDStep2', 'After logging in on the authorization page, this will complete automatically.') }}
              </p>
            </div>

            <div class="flex items-center justify-center gap-2 text-sm text-gray-600 dark:text-gray-400">
              <Icon name="refresh" size="sm" class="animate-spin" />
              <span>{{ t('admin.kiro.builderIDPolling', '等待授权完成...') }}</span>
            </div>
          </div>
        </div>

        <!-- OAuth mode -->
        <div v-if="mode === 'oauth'" class="space-y-4">
          <div v-if="!authURL">
            <p class="mb-2 text-sm text-gray-600 dark:text-gray-300">
              {{ t('admin.kiro.oauthStep1Intro', 'Click the button below to generate a Cognito authorize URL. The server will create a PKCE + state pair.') }}
            </p>
            <button
              type="button"
              class="btn btn-primary w-full"
              :disabled="loading"
              @click="generateAuthURL"
            >
              <Icon v-if="loading" name="refresh" size="sm" class="animate-spin" />
              {{ t('admin.kiro.generateAuthUrl', 'Generate Auth URL') }}
            </button>
          </div>

          <div v-else class="space-y-4">
            <div
              class="rounded-lg border border-indigo-200 bg-indigo-50 p-4 dark:border-indigo-700 dark:bg-indigo-900/20"
            >
              <p class="mb-2 text-sm font-medium text-indigo-900 dark:text-indigo-200">
                {{ t('admin.kiro.oauthStep2Intro', 'Step 1. Open the URL in a new tab and complete Google login.') }}
              </p>
              <div class="mb-2 break-all rounded bg-white p-2 font-mono text-xs text-gray-700 dark:bg-dark-800 dark:text-gray-300">
                {{ authURL }}
              </div>
              <div class="flex gap-2">
                <button type="button" class="btn btn-secondary btn-sm flex-1" @click="copyAuthURL">
                  <Icon name="copy" size="sm" />
                  {{ copied ? t('common.copied', 'Copied') : t('common.copy', 'Copy') }}
                </button>
                <a
                  :href="authURL"
                  target="_blank"
                  rel="noopener"
                  class="btn btn-primary btn-sm flex-1"
                >
                  <Icon name="externalLink" size="sm" />
                  {{ t('admin.kiro.openInBrowser', 'Open in New Tab') }}
                </a>
              </div>
            </div>

            <div>
              <label class="input-label">
                {{ t('admin.kiro.oauthStep3Intro', 'Step 2. Paste the full callback URL from the browser address bar.') }}
              </label>
              <textarea
                v-model="callbackURL"
                rows="3"
                class="input font-mono text-xs"
                placeholder="https://app.kiro.dev/signin/oauth?code=...&state=..."
              ></textarea>
              <p class="input-hint">
                {{ t('admin.kiro.callbackHint', 'If the page redirects too fast, open F12 Network, tick Preserve log, then copy the signin/oauth request URL.') }}
              </p>
            </div>
          </div>

          <div class="grid gap-3 border-t border-gray-200 pt-4 dark:border-dark-600 sm:grid-cols-2">
            <div>
              <label class="input-label">{{ t('admin.accounts.name', 'Account Name') }}</label>
              <input v-model="name" type="text" class="input" placeholder="Kiro OAuth Account" />
            </div>
            <div>
              <label class="input-label">{{ t('admin.accounts.concurrency', 'Concurrency') }}</label>
              <input v-model.number="concurrency" type="number" min="1" class="input" />
            </div>
          </div>

          <div class="flex gap-2">
            <button type="button" class="btn btn-secondary flex-1" @click="handleClose">
              {{ t('common.cancel', 'Cancel') }}
            </button>
            <button
              type="button"
              class="btn btn-primary flex-1"
              :disabled="loading || !callbackURL.trim() || !sessionID"
              @click="submitOAuth"
            >
              <Icon v-if="loading" name="refresh" size="sm" class="animate-spin" />
              {{ t('admin.kiro.submitOauth', 'Exchange & Create Account') }}
            </button>
          </div>
        </div>

        <!-- Manual mode -->
        <div v-if="mode === 'manual'" class="space-y-4">
          <div>
            <label class="input-label">{{ t('admin.kiro.accountJson', 'Account JSON') }}</label>
            <textarea
              v-model="manualJSON"
              rows="5"
              class="input font-mono text-xs"
              placeholder='{"accessToken":"aoa...","refreshToken":"aor...","provider":"BuilderId","clientId":"...","clientSecret":"..."}'
              @blur="applyManualJSON(false)"
            ></textarea>
            <div class="mt-2 flex items-center gap-2">
              <button type="button" class="btn btn-secondary btn-sm" :disabled="!manualJSON.trim()" @click="applyManualJSON(true)">
                {{ t('admin.kiro.parseJson', 'Parse JSON') }}
              </button>
              <span v-if="manualParsedSummary" class="text-xs text-green-600 dark:text-green-400">{{ manualParsedSummary }}</span>
            </div>
          </div>
          <div>
            <label class="input-label">Access Token <span class="text-red-500">*</span></label>
            <textarea
              v-model="manualAccessToken"
              rows="3"
              class="input font-mono text-xs"
              placeholder="aoaAAAAAGn..."
              @blur="applyAccessTokenJSON(false)"
            ></textarea>
          </div>
          <div>
            <label class="input-label">Refresh Token</label>
            <textarea
              v-model="manualRefreshToken"
              rows="3"
              class="input font-mono text-xs"
              placeholder="aorAAAAAGp..."
            ></textarea>
          </div>
          <div>
            <label class="input-label">Profile ARN</label>
            <input
              v-model="manualProfileArn"
              type="text"
              class="input font-mono text-xs"
              placeholder="arn:aws:codewhisperer:us-east-1:xxx:profile/xxx"
            />
          </div>
          <div>
            <label class="input-label">
              {{ t('admin.accounts.kiro.machineIdLabel', 'Machine ID (设备指纹)') }}
            </label>
            <input
              v-model="manualMachineId"
              type="text"
              class="input font-mono text-xs"
              placeholder="64-char hex, e.g. f3a1b2...   (from Kiro-account-manager export)"
            />
            <p class="input-hint text-xs text-amber-600 dark:text-amber-400">
              {{ t('admin.accounts.kiro.machineIdHint', '从 Kiro-account-manager 的账号 JSON 中复制 machineId 字段，留空则用账号 ID 派生 sha256 兜底（不推荐，AWS 易识别）。') }}
            </p>
          </div>
          <div class="grid gap-3 sm:grid-cols-2">
            <div>
              <label class="input-label">Email</label>
              <input v-model="manualEmail" type="email" class="input" placeholder="user@gmail.com" />
            </div>
            <div>
              <label class="input-label">CSRF Token</label>
              <input v-model="manualCSRF" type="text" class="input font-mono text-xs" />
            </div>
          </div>
          <div class="grid gap-3 sm:grid-cols-2">
            <div>
              <label class="input-label">{{ t('admin.accounts.name', 'Account Name') }}</label>
              <input v-model="name" type="text" class="input" />
            </div>
            <div>
              <label class="input-label">{{ t('admin.accounts.concurrency', 'Concurrency') }}</label>
              <input v-model.number="concurrency" type="number" min="1" class="input" />
            </div>
          </div>

          <div class="flex gap-2">
            <button type="button" class="btn btn-secondary flex-1" @click="handleClose">
              {{ t('common.cancel', 'Cancel') }}
            </button>
            <button
              type="button"
              class="btn btn-primary flex-1"
              :disabled="loading || !manualCanSubmit"
              @click="submitManual"
            >
              <Icon v-if="loading" name="refresh" size="sm" class="animate-spin" />
              {{ t('admin.kiro.submitManual', 'Create Account') }}
            </button>
          </div>
        </div>

        <div
          v-if="errorMessage"
          class="mt-3 rounded-md border border-red-200 bg-red-50 p-3 text-sm text-red-700 dark:border-red-800 dark:bg-red-900/30 dark:text-red-300"
        >
          {{ errorMessage }}
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { apiClient } from '@/api'
import Icon from '@/components/icons/Icon.vue'

const props = defineProps<{
  modelValue?: boolean
  /** 嵌入到父组件内部使用时为 true；为 false 时渲染为独立 modal（向后兼容） */
  embedded?: boolean
  /** Embedded mode: pass fields from parent to avoid duplicate input */
  externalName?: string
  externalNotes?: string | null
  externalConcurrency?: number
  externalPriority?: number
  externalProxyId?: number | null
  externalGroupIds?: number[]
}>()
const emit = defineEmits<{
  (e: 'update:modelValue', value: boolean): void
  (e: 'created', account: unknown): void
  /** embedded 模式下父组件可用于关闭或重置 */
  (e: 'close'): void
}>()

const { t } = useI18n()

const mode = ref<'oauth' | 'manual' | 'builderid'>('builderid')
const loading = ref(false)
const errorMessage = ref('')
const copied = ref(false)

// Builder ID state
const builderIDSessionID = ref('')
const builderIDUserCode = ref('')
const builderIDVerificationURI = ref('')
const builderIDInterval = ref(5)
let builderIDPollTimer: ReturnType<typeof setInterval> | null = null

// OAuth flow state
const authURL = ref('')
const sessionID = ref('')
const callbackURL = ref('')

// Manual mode state
const manualJSON = ref('')
const manualAccessToken = ref('')
const manualRefreshToken = ref('')
const manualProfileArn = ref('')
const manualEmail = ref('')
const manualCSRF = ref('')
const manualUserID = ref('')
const manualClientID = ref('')
const manualClientSecret = ref('')
const manualRegion = ref('')
const manualProvider = ref('')
const manualAuthMethod = ref('')
// manualMachineId holds the per-account device fingerprint (real
// MachineGuid harvested from Kiro-account-manager, or a 64-char hex
// string). When empty the gateway falls back to a sha256("kiro-device-<id>")
// derivative — stable but distinguishable from a real Kiro IDE client,
// which raises ban risk. Always paste the real value for production.
const manualMachineId = ref('')
const manualExpiresAt = ref<number | string | undefined>(undefined)
const manualParsedSummary = ref('')
const manualCanSubmit = computed(() => Boolean(manualAccessToken.value.trim()))

// Common fields
const name = ref('')
const concurrency = ref(3)

watch(
  () => props.modelValue,
  (v) => {
    if (!v) {
      resetState()
    }
  }
)

function resetState() {
  mode.value = 'builderid'
  loading.value = false
  errorMessage.value = ''
  copied.value = false
  authURL.value = ''
  sessionID.value = ''
  callbackURL.value = ''
  manualJSON.value = ''
  manualAccessToken.value = ''
  manualRefreshToken.value = ''
  manualProfileArn.value = ''
  manualEmail.value = ''
  manualCSRF.value = ''
  manualUserID.value = ''
  manualClientID.value = ''
  manualClientSecret.value = ''
  manualRegion.value = ''
  manualProvider.value = ''
  manualAuthMethod.value = ''
  manualMachineId.value = ''
  manualExpiresAt.value = undefined
  manualParsedSummary.value = ''
  name.value = ''
  concurrency.value = 3
  // Builder ID
  stopBuilderIDPolling()
  builderIDSessionID.value = ''
  builderIDUserCode.value = ''
  builderIDVerificationURI.value = ''
  builderIDInterval.value = 5
}

function stopBuilderIDPolling() {
  if (builderIDPollTimer) {
    clearInterval(builderIDPollTimer)
    builderIDPollTimer = null
  }
}

async function startBuilderIDLogin() {
  loading.value = true
  errorMessage.value = ''
  try {
    const { data } = await apiClient.post('/admin/kiro/builderid/start', {})
    const resp = data as { session_id: string; user_code: string; verification_uri: string; interval: number; expires_in: number }
    builderIDSessionID.value = resp.session_id
    builderIDUserCode.value = resp.user_code
    builderIDVerificationURI.value = resp.verification_uri
    builderIDInterval.value = resp.interval || 5

    // Auto-open the verification URI
    window.open(resp.verification_uri, '_blank', 'noopener')

    // Start polling
    startBuilderIDPolling()
  } catch (e: unknown) {
    errorMessage.value = extractError(e)
  } finally {
    loading.value = false
  }
}

function startBuilderIDPolling() {
  stopBuilderIDPolling()
  builderIDPollTimer = setInterval(pollBuilderIDStatus, builderIDInterval.value * 1000)
}

async function pollBuilderIDStatus() {
  if (!builderIDSessionID.value) {
    stopBuilderIDPolling()
    return
  }
  try {
    const { data } = await apiClient.post('/admin/kiro/builderid/poll', {
      session_id: builderIDSessionID.value
    })
    const resp = data as {
      status: string
      access_token?: string
      refresh_token?: string
      client_id?: string
      client_secret?: string
      region?: string
      expires_in?: number
      interval?: number
    }

    if (resp.status === 'pending') return
    if (resp.status === 'slow_down') {
      // Increase interval
      builderIDInterval.value = resp.interval || builderIDInterval.value + 5
      stopBuilderIDPolling()
      builderIDPollTimer = setInterval(pollBuilderIDStatus, builderIDInterval.value * 1000)
      return
    }
    if (resp.status === 'completed') {
      stopBuilderIDPolling()
      // Create account
      await createAccountFromBuilderID(resp)
    }
  } catch (e: unknown) {
    stopBuilderIDPolling()
    errorMessage.value = extractError(e)
  }
}

async function createAccountFromBuilderID(tokenData: {
  access_token?: string
  refresh_token?: string
  client_id?: string
  client_secret?: string
  region?: string
  expires_in?: number
  profile_arn?: string
}) {
  loading.value = true
  errorMessage.value = ''
  try {
    const resolvedName = props.embedded
      ? (props.externalName?.trim() || name.value || undefined)
      : (name.value || undefined)
    const resolvedConcurrency = props.embedded
      ? (props.externalConcurrency ?? concurrency.value ?? 3)
      : (concurrency.value || 3)

    const { data } = await apiClient.post('/admin/kiro/builderid/create-account', {
      access_token: tokenData.access_token,
      refresh_token: tokenData.refresh_token,
      client_id: tokenData.client_id,
      client_secret: tokenData.client_secret,
      region: tokenData.region || 'us-east-1',
      expires_in: tokenData.expires_in,
      profile_arn: tokenData.profile_arn || '',
      name: resolvedName,
      concurrency: resolvedConcurrency,
      priority: props.embedded ? (props.externalPriority ?? undefined) : undefined,
      group_ids: props.embedded ? (props.externalGroupIds ?? undefined) : undefined,
      notes: props.embedded ? (props.externalNotes ?? undefined) : undefined,
    })
    emit('created', data)
    handleClose()
  } catch (e: unknown) {
    errorMessage.value = extractError(e)
  } finally {
    loading.value = false
  }
}

function handleClose() {
  if (props.embedded) {
    emit('close')
    resetState()
    return
  }
  emit('update:modelValue', false)
}

async function copyAuthURL() {
  try {
    await navigator.clipboard.writeText(authURL.value)
    copied.value = true
    setTimeout(() => (copied.value = false), 2000)
  } catch {
    /* ignore */
  }
}

async function generateAuthURL() {
  loading.value = true
  errorMessage.value = ''
  try {
    // apiClient interceptor already unwraps { code, message, data } envelope to response.data
    const { data } = await apiClient.post('/admin/kiro/oauth/auth-url', {})
    authURL.value = (data as { auth_url?: string })?.auth_url ?? ''
    sessionID.value = (data as { session_id?: string })?.session_id ?? ''
    if (!authURL.value || !sessionID.value) {
      errorMessage.value = t('admin.kiro.authUrlEmpty', 'Server returned no auth URL.') as string
    }
  } catch (e: unknown) {
    errorMessage.value = extractError(e)
  } finally {
    loading.value = false
  }
}

async function submitOAuth() {
  if (!sessionID.value || !callbackURL.value.trim()) return
  loading.value = true
  errorMessage.value = ''
  try {
    const resolvedName = props.embedded
      ? (props.externalName?.trim() || name.value || undefined)
      : (name.value || undefined)
    const resolvedConcurrency = props.embedded
      ? (props.externalConcurrency ?? concurrency.value ?? 3)
      : (concurrency.value || 3)
    const { data } = await apiClient.post('/admin/kiro/create-from-oauth', {
      session_id: sessionID.value,
      callback_url: callbackURL.value.trim(),
      name: resolvedName,
      concurrency: resolvedConcurrency,
      priority: props.embedded ? (props.externalPriority ?? undefined) : undefined,
      proxy_id: props.embedded ? (props.externalProxyId ?? undefined) : undefined,
      group_ids: props.embedded ? (props.externalGroupIds ?? undefined) : undefined,
      notes: props.embedded ? (props.externalNotes ?? undefined) : undefined
    })
    // apiClient interceptor already unwraps envelope to response.data
    emit('created', data)
    handleClose()
  } catch (e: unknown) {
    errorMessage.value = extractError(e)
  } finally {
    loading.value = false
  }
}

async function submitManual() {
  applyAccessTokenJSON(false)
  if (!manualAccessToken.value.trim() && manualJSON.value.trim()) {
    applyManualJSON(true)
  }
  if (!manualCanSubmit.value) return
  loading.value = true
  errorMessage.value = ''
  try {
    const resolvedName = props.embedded
      ? (props.externalName?.trim() || name.value || undefined)
      : (name.value || undefined)
    const resolvedConcurrency = props.embedded
      ? (props.externalConcurrency ?? concurrency.value ?? 3)
      : (concurrency.value || 3)
    const { data } = await apiClient.post('/admin/kiro/create-from-tokens', {
      access_token: manualAccessToken.value.trim(),
      refresh_token: manualRefreshToken.value.trim(),
      profile_arn: manualProfileArn.value.trim(),
      email: manualEmail.value.trim() || undefined,
      csrf_token: manualCSRF.value.trim() || undefined,
      user_id: manualUserID.value.trim() || undefined,
      client_id: manualClientID.value.trim() || undefined,
      client_secret: manualClientSecret.value.trim() || undefined,
      region: manualRegion.value.trim() || undefined,
      provider: manualProvider.value.trim() || undefined,
      auth_method: manualAuthMethod.value.trim() || undefined,
      machine_id: manualMachineId.value.trim() || undefined,
      expires_at: manualExpiresAt.value,
      name: resolvedName,
      concurrency: resolvedConcurrency,
      priority: props.embedded ? (props.externalPriority ?? undefined) : undefined,
      proxy_id: props.embedded ? (props.externalProxyId ?? undefined) : undefined,
      group_ids: props.embedded ? (props.externalGroupIds ?? undefined) : undefined,
      notes: props.embedded ? (props.externalNotes ?? undefined) : undefined
    })
    // apiClient interceptor already unwraps envelope to response.data
    emit('created', data)
    handleClose()
  } catch (e: unknown) {
    errorMessage.value = extractError(e)
  } finally {
    loading.value = false
  }
}

type KiroPastedAccount = Record<string, unknown>

function pickString(obj: KiroPastedAccount, ...keys: string[]): string {
  for (const key of keys) {
    const value = obj[key]
    if (typeof value === 'string' && value.trim()) return value.trim()
  }
  return ''
}

function parseExpiresAt(value: unknown): number | string | undefined {
  if (typeof value === 'number' && Number.isFinite(value)) {
    return value > 1_000_000_000_000 ? Math.floor(value / 1000) : Math.floor(value)
  }
  if (typeof value !== 'string') return undefined
  const raw = value.trim()
  if (!raw) return undefined
  if (/^\d+(\.\d+)?$/.test(raw)) {
    const n = Number(raw)
    return n > 1_000_000_000_000 ? Math.floor(n / 1000) : Math.floor(n)
  }
  const normalized = raw.includes('T') ? raw : raw.replace(/\//g, '-').replace(' ', 'T')
  const ms = Date.parse(normalized)
  if (Number.isFinite(ms)) return Math.floor(ms / 1000)
  return raw
}

function applyManualJSON(showError: boolean) {
  const raw = manualJSON.value.trim()
  if (!raw) return false
  try {
    const parsed = JSON.parse(raw) as KiroPastedAccount
    manualAccessToken.value = pickString(parsed, 'access_token', 'accessToken') || manualAccessToken.value
    manualRefreshToken.value = pickString(parsed, 'refresh_token', 'refreshToken') || manualRefreshToken.value
    manualProfileArn.value = pickString(parsed, 'profile_arn', 'profileArn') || manualProfileArn.value
    manualEmail.value = pickString(parsed, 'email') || manualEmail.value
    manualCSRF.value = pickString(parsed, 'csrf_token', 'csrfToken') || manualCSRF.value
    manualUserID.value = pickString(parsed, 'user_id', 'userId') || manualUserID.value
    manualClientID.value = pickString(parsed, 'client_id', 'clientId') || manualClientID.value
    manualClientSecret.value = pickString(parsed, 'client_secret', 'clientSecret') || manualClientSecret.value
    manualRegion.value = pickString(parsed, 'region') || manualRegion.value
    manualProvider.value = pickString(parsed, 'provider') || manualProvider.value
    manualAuthMethod.value = pickString(parsed, 'auth_method', 'authMethod') || manualAuthMethod.value
    // Kiro-account-manager exports the field as `machineId` (camelCase);
    // the upstream Kiro IDE / kiro-cli legacy dumps use `machine_id`.
    // Accept both so any export source works.
    manualMachineId.value = pickString(parsed, 'machine_id', 'machineId') || manualMachineId.value
    manualExpiresAt.value = parseExpiresAt(parsed.expires_at ?? parsed.expiresAt)

    const label = pickString(parsed, 'label', 'name')
    if (!name.value && label) name.value = label

    const provider = manualProvider.value || 'Kiro'
    const accountType = manualClientID.value && manualClientSecret.value ? 'Builder ID' : provider
    manualParsedSummary.value = `${accountType} parsed`
    errorMessage.value = ''
    return true
  } catch {
    manualParsedSummary.value = ''
    if (showError) {
      errorMessage.value = t('admin.kiro.invalidAccountJson', 'Invalid account JSON.') as string
    }
    return false
  }
}

function applyAccessTokenJSON(showError: boolean) {
  const raw = manualAccessToken.value.trim()
  if (!raw.startsWith('{')) return false
  manualJSON.value = raw
  return applyManualJSON(showError)
}

function extractError(e: unknown): string {
  if (typeof e === 'object' && e !== null) {
    const anyE = e as {
      response?: { data?: { message?: string; error?: string } }
      message?: string
    }
    return (
      anyE.response?.data?.message ||
      anyE.response?.data?.error ||
      anyE.message ||
      String(e)
    )
  }
  return String(e)
}
</script>
