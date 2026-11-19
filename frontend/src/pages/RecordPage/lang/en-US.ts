import type { Messages } from '@/i18n';

// RecordPage English copy. Mounted under the `record.*` namespace.
export const recordPageEnUS: Messages = {
  record: {
    eyebrow: 'Record',
    title: 'Choose an audio source to transcribe',
    subtitle: 'Capture Chinese speech from a selected app. You can usually transcribe a recording later from the library.',
    noAudio: {
      title: 'Audio is not saved for this session',
      hint: 'Audio is used only for this online transcription. It cannot be played back or exported. If transcription fails, there is no recording to retry with.',
      failed: 'Online transcription failed. No audio was saved, so this session cannot be transcribed again.',
    },

    caps: {
      title: 'Platform capture capabilities',
      loading: 'Reading platform capabilities…',
      processOn: 'Per-application capture is supported: only the selected app is recorded.',
      processOff: 'This platform or OS version does not support per-application capture, so only full-device mixing is available.',
      reasonLabel: 'Reason',
      deviceOn: 'Device sources use loopback and include audio from every application.',
      deviceOff: 'No loopback device is available on this platform.',
      permission: 'System audio permission must be granted before app audio can be captured.',
      platformLabel: 'Platform',
    },

    // Recording main states (the values that appear on the `state` field).
    // ST-07 (degraded) is not one of them: it is an orthogonal boolean flag shown in the model source area.
    state: {
      idle: 'Idle',
      preparing: 'Preparing',
      recording: 'Recording',
      paused: 'Paused',
      finalizing: 'Finalizing',
      completed: 'Completed',
      interrupted: 'Interrupted',
      failed: 'Failed',
    },

    controls: {
      title: 'Recording controls',
      hint: 'Buttons follow the recording state machine: start from idle, pause or stop while recording, resume or stop while paused.',
      start: 'Start recording',
      startHint: 'Select an available audio source above before starting.',
      pause: 'Pause',
      resume: 'Resume',
      stop: 'Stop',
      exportAudio: 'Save recording as…',
      recover: 'Repair session',
    },

    metrics: {
      levelLabel: 'Level',
      levelAria: 'Current capture level',
      peakLabel: 'Peak {{percent}}%',
      elapsedLabel: 'Recorded',
      bytesLabel: 'Written',
      sourceLabel: 'Source',
      silentHint: 'No audio detected for {{seconds}} seconds. Check that the application is playing sound.',
    },

    model: {
      title: 'Model source',
      hint: 'Switch before recording starts; it is locked while recording because the backend rejects the switch.',
      lockedHint: 'The model source cannot be changed while recording. Stop recording first.',
      local: 'Local',
      online: 'Online',
      aria: 'Choose model source',
      loading: 'Reading engine status…',
      degraded: 'Fell back to local',
      throttled: 'Throttled {{level}}',
      suspended: 'Inference suspended',
      lagging: 'Transcription behind',
      failedHint: 'The inference engine failed to load, so transcription is unavailable for now. Recording is not affected.',
      state: {
        loading: 'Engine loading',
        ready: 'Engine ready',
        failed: 'Engine unavailable',
        degraded: 'Engine degraded',
        paused: 'Engine paused',
      },
    },

    sources: {
      title: 'Audio sources',
      refresh: 'Refresh sources',
      refreshing: 'Refreshing audio sources…',
      groupApplications: 'Applications',
      groupDevices: 'Devices',
      emptyApplications: 'No application is currently producing audio.',
      emptyDevices: 'No loopback device available.',
      present: 'Available',
      absent: 'Not running',
      producing: 'Producing audio',
      childCount: '{{count}} child processes',
      defaultDevice: 'Default device',
      absentTooltip: 'This source is unavailable right now (process exited or device disconnected) and cannot be selected.',
      selected: 'Selected',
      selectAria: 'Select audio source: {{name}}',
      unavailableBanner: 'The selected source "{{name}}" is no longer available. Please choose another one.',
      unknownUnavailable: 'The previously selected source is no longer available. Please choose another one.',
      loadFailed: 'Failed to load audio sources.',
      warn: {
        'exclusive-mode': 'This source may be held in exclusive mode, so no audio will be heard while recording.',
        'process-loopback-unavailable': 'This machine cannot capture per-application audio, so this source cannot be recorded. Use a device source instead (it includes audio from all applications).',
      },
      lockedHint: 'The audio source cannot be changed while recording.',
      lockedTooltip: 'Recording is in progress and the audio source is bound to the current session. Stop recording to change it.',
    },

    export: {
      dialogTitle: 'Save recording as',
      fileBaseName: 'recording',
      filterWav: 'WAV audio (*.wav)',
      filterFlac: 'FLAC audio (*.flac)',
      doneTitle: 'Recording saved',
      doneMessage: 'The recording was saved to:\n{{path}}',
    },

    // Live transcription (FR-03-10, off by default): the whole block is hidden when off.
    live: {
      title: 'Live transcription',
      stateOn: 'Transcribing live',
      stateUnavailable: 'Unavailable',
      idleHint: 'Recognized text will appear here once recording starts.',
      waiting: 'Waiting for speech…',
      capErrors: {
        ERR_LOCAL_UNAVAILABLE: 'This build has no streaming engine, so live transcription is unavailable.',
        ERR_MODEL_NOT_FOUND: 'Streaming model bundle is missing: download a streaming Chinese model on the Models page.',
        ERR_MODEL_CORRUPT: 'The streaming model bundle is corrupt; please download it again.',
        ERR_ENGINE_LOAD: 'The streaming engine failed to load, so this session will not show live text (recording is unaffected).',
      },
    },

    // Error copy keyed by backend error code (internal/apierr); falls back to errors.unknown.
    errors: {
      unknown: 'Operation failed: {{message}}',
      dismiss: 'Dismiss message',
      ERR_CAPTURE_PERMISSION: 'System audio permission is missing. Grant this app microphone access in system settings and retry.',
      ERR_CAPTURE_UNAVAILABLE: 'The system audio capture service is unavailable. Check the system audio device and retry.',
      ERR_CAPTURE_DEVICE_LOST: 'The audio device was disconnected. Reconnect it and refresh the list.',
      ERR_SOURCE_NOT_FOUND: 'This audio source no longer exists. Refresh the list and choose again.',
      ERR_INVALID_ARGUMENT: 'The request arguments were invalid. Refresh the list and retry.',
      ERR_OFFLINE_ENFORCED: 'Offline mode is on, so this operation will not make any network request.',
      ERR_PATH_UNWRITABLE: 'The target path is not writable. Choose another location.',
      ERR_STATE_FORBIDDEN: 'This action is not allowed in the current state. Check the recording state and retry.',
      emptyRecording: 'No audio was captured (shorter than 1 second or silent throughout). The session is kept in the library. Make sure the selected source is playing sound, then record again.',
      ERR_DISK_FULL: 'Not enough disk space, so recording stopped. Free some space and start again.',
      ERR_DB_WRITE: 'Writing to the database failed. Please retry.',
      ERR_NOT_FOUND: 'The session could not be found; it may have been deleted.',
      ERR_ENGINE_NOT_READY: 'The inference engine is not ready yet. Please retry shortly.',
      ERR_MODEL_NOT_FOUND: 'The current model is unavailable. Download or switch models on the Models page.',
    },
  },
};
