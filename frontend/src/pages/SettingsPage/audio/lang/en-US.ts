import type { Messages } from '@/i18n';

// 音频子页面文案（英文）。namespace: settings.audio.*
export const audioEnUS: Messages = {
  settings: {
    audio: {
      title: 'Audio & recording',
      hint: 'Choose the default source, speaker analysis limit, and naming for new sessions. Recording only captures and writes audio; transcription is a separate task.',
      defaults: {
        source: 'Default audio source',
        sourceHint: 'Shared by the recording page and recording shortcut; an active recording keeps its source.',
        noSource: 'Choose an audio source',
        clearSource: 'Clear default audio source',
        noSources: 'No audio sources available',
        unavailableOption: 'Saved audio source (unavailable)',
        unavailable: 'The default source is unavailable. Choose another one; recording will not switch sources automatically.',
        sourcesFailed: 'Could not load audio sources. Refresh to retry.',
        refresh: 'Refresh audio sources',
        sensitivity: 'Speech detection sensitivity',
        sensitivityHint: 'Applies to the next recording. Noisy mode requires louder speech and may miss quiet voices. Neither mode changes the recording.',
        standard: 'Standard',
        noisy: 'Noisy environment',
        maxSpeakers: 'Speaker limit',
        maxSpeakersHint: 'Applies to the next speaker analysis; existing results are unchanged.',
        auto: 'Automatic',
        speakers: '{{count}} speakers',
        autoName: 'Name sessions automatically',
        autoNameHint: 'New sessions use “Date Time · Source app”. Turn off to leave new sessions unnamed; existing titles stay unchanged.',
        format: 'Recording file format',
        formatHint: 'WAV is more widely compatible. FLAC uses lossless compression. Applies to the next recording.',
      },
      live: {
        title: 'Live transcription',
        desc: 'Shows text while recording: partials are overwritten as you go, and each finished sentence is finalized for good.',
        stateOn: 'On',
        stateOff: 'Off',
        switchLabel: 'Enable live transcription (text while recording)',
        noteDefault: 'Off by default: recording works without it, and you can transcribe later from the library.',
        noteNextSession: 'Applies to the next session only: stop recording before switching it.',
        noteDeferred: 'Uses extra CPU: live transcription keeps a streaming model resident, and may be downgraded (second-pass refinement off) on a busy machine.',
        capLabel: 'Streaming capability',
        capReady: 'Available ({{engine}})',
        capReadyUnknown: 'streaming model',
        capChecking: 'Checking…',
        capUnavailable: 'Unavailable',
        capErrors: {
          ERR_LOCAL_UNAVAILABLE: 'This build has no streaming engine, so live transcription is unavailable.',
          ERR_MODEL_NOT_FOUND: 'Streaming model bundle is missing: download a streaming Chinese model on the Models page.',
          ERR_MODEL_CORRUPT: 'The streaming model bundle is corrupt; please download it again.',
          ERR_ENGINE_LOAD: 'The streaming engine failed to load; see diagnostics.',
        },
        enabledButUnavailable: 'The switch is on, but no streaming model is available, so this session will not show live text.',
      },
      feedback: {
        saving: 'Saving…',
        saved: 'Saved.',
        ERR_STATE_FORBIDDEN: 'You cannot switch live transcription while recording. Stop recording first (it applies to the next session).',
      },
      errors: {
        ERR_DB_WRITE: 'Save failed: database write error.',
      },
    },
  },
};
