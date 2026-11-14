import type { Messages } from '@/i18n';

// LibraryPage English copy. Mounted under the `library.*` namespace.
export const libraryPageEnUS: Messages = {
  library: {
    eyebrow: 'Library',
    title: 'Recorded sessions',
    subtitle: 'Every recording instance is here: search it, rename it, export the text, or start transcription for recordings that have not been transcribed yet.',

    search: {
      title: 'Search and filters',
      reset: 'Reset filters',
      refresh: 'Refresh list',
      keywordLabel: 'Keyword',
      keywordPlaceholder: 'Search titles or transcribed text',
      keywordAria: 'Search sessions by keyword',
      fromLabel: 'From date',
      fromAria: 'Filter sessions started on or after',
      toLabel: 'To date',
      toAria: 'Filter sessions started on or before',
      sourceLabel: 'Source application',
      sourcePlaceholder: 'All applications',
      sourceAria: 'Filter by source application',
      statusLabel: 'Transcription status',
      statusPlaceholder: 'All statuses',
      statusAria: 'Filter by transcription status',
      minDurationLabel: 'Min duration',
      minDurationAria: 'Filter sessions at least this long (seconds)',
      maxDurationLabel: 'Max duration',
      maxDurationAria: 'Filter sessions at most this long (seconds)',
      secondsSuffix: 's',
      durationHint: 'The duration range only filters the current page (the list API does not support duration queries yet).',
    },

    list: {
      title: 'Sessions',
      countLabel: '{{count}} total',
      durationFiltered: '{{shown}} on this page',
      pageSizeAria: 'Rows per page',
      pageSizeOption: '{{count}} per page',
      emptyTitle: 'No sessions recorded yet',
      emptyDescription: 'Go to the Record page, pick an application audio source and start recording; the session shows up here when it ends.',
      emptyFilteredTitle: 'No sessions match the filters',
      emptyFilteredDescription: 'Try a wider date range, a shorter keyword, or reset the filters.',
      degraded: 'Degraded',
      online: 'Online model',
      sourceLabel: 'Source',
      segmentLabel: '{{count}} segments',
      progressAria: 'Transcription progress',
      progressText: '{{percent}}% transcribed',
      startTranscription: 'Start transcription',
      retryTranscription: 'Retry transcription',
      cancelTranscription: 'Cancel transcription',
      export: 'Export text',
      rename: 'Rename',
      delete: 'Delete session',
      speakers: 'Speakers and roles',
      playback: 'Play back',
    },

    // Transcription statuses (a separate axis, orthogonal to the recording status).
    transcription: {
      none: 'Not transcribed',
      queued: 'Queued',
      running: 'Transcribing',
      done: 'Transcribed',
      failed: 'Transcription failed',
    },

    rename: {
      title: 'Rename session',
      label: 'Title',
      placeholder: 'Enter a new session title',
      cancel: 'Cancel',
      submit: 'Save',
    },

    delete: {
      title: 'Delete session',
      message: 'This removes the session "{{title}}" and its transcript. It cannot be undone.',
      alsoAudio: 'Also delete the audio file',
      alsoAudioHint: 'Clear this to keep the audio file on disk and only remove the record.',
      cancel: 'Cancel',
      submit: 'Delete',
      confirmTitle: 'Confirm deletion',
      confirmMessage: 'Deleting cannot be undone. Delete "{{title}}"?',
      confirmOk: 'Delete',
      confirmCancel: 'Cancel',
      audioLeftTitle: 'Records deleted',
      audioLeftMessage: '{{count}} audio files could not be deleted and remain on disk; you can clean them up later.',
      doneTitle: 'Deleted',
      doneMessage: 'Deleted {{count}} sessions, including {{audio}} audio files.',
    },

    export: {
      dialogTitle: 'Export session text',
      filterTxt: 'Plain text (*.txt)',
      filterSrt: 'Subtitles (*.srt)',
      filterCsv: 'CSV (*.csv)',
      filterXlsx: 'Excel (*.xlsx)',
      formatTxt: 'Export as plain text (.txt)',
      formatSrt: 'Export as subtitles (.srt)',
      formatCsv: 'Export as CSV (.csv)',
      formatXlsx: 'Export as Excel (.xlsx)',
      doneTitle: 'Export complete',
      doneMessage: 'The text was exported to:\n{{path}}',
    },

    // Speakers / roles sidebar in the session detail (FR-06 UI side).
    speakers: {
      title: 'Speakers / roles',
      subtitle: 'Renaming a role applies to every session; merging and splitting act on the voice identity itself.',
      close: 'Close speakers sidebar',
      refresh: 'Refresh speakers',
      loading: 'Loading speakers…',

      analyze: {
        title: 'Speaker diarization',
        hint: 'Groups segments by voice and can be run again (it overwrites the previous result).',
        run: 'Start diarization',
        rerun: 'Run again',
        running: 'Diarizing…',
        noModelTitle: 'Diarization unavailable',
        noModel: 'No speaker model is installed on this machine. Download one on the Models page first. Renaming and assigning roles still works.',
        resultTitle: 'Diarization complete',
        resultDismiss: 'Hide diarization result',
        resultSummary: '{{speakers}} speakers · {{assigned}} segments assigned · {{unidentified}} unidentified',
        resultDetail: '{{segments}} segments processed; {{tooShort}} skipped as too short, {{failed}} could not be read.',
        modelLabel: 'Speaker model: {{model}}',
      },

      identified: {
        title: 'Identified speakers',
        empty: 'No speakers separated in this session yet. Transcribe the recording first, then start diarization.',
      },

      rows: {
        unnamed: 'Unnamed role',
        count: '{{count}} segments',
        speech: '{{duration}} speech',
        rename: 'Rename role',
        split: 'Split out segments',
        assign: 'Assign to role',
        merge: 'Merge speakers',
        mergeInto: 'Merge this speaker into…',
        nameLabel: 'Role name',
        namePlaceholder: 'Name this speaker',
        save: 'Save',
        cancel: 'Cancel',
      },

      unidentified: {
        title: 'Unidentified speech',
        description:
          'These segments have no usable voice identity (overlapping speech, too short, or extraction failed), so they cannot be assigned or split. Run diarization again to improve them.',
      },

      merge: {
        title: 'Merge two speakers',
        description:
          'After merging, every segment of the source speaker belongs to the target speaker, the voice embeddings are fused by weight, and the source identity is deleted.',
        irreversible: 'Merging cannot be undone automatically (only by reassigning or re-running diarization).',
        fromLabel: 'Speaker to merge away',
        toLabel: 'Merge into',
        placeholder: 'Pick a speaker',
        needTwo: 'Merging needs at least two identified speakers.',
        cancel: 'Cancel',
        submit: 'Merge',
        confirmTitle: 'Confirm merging speakers',
        confirmMessage:
          '"{{from}}" will be merged into "{{to}}". The source speaker is deleted permanently and this cannot be undone automatically. Continue?',
        confirmOk: 'Merge',
        confirmCancel: 'Cancel',
        doneTitle: 'Merged',
        doneMessage: 'The speakers were merged; affected segments and past sessions are updated.',
      },

      split: {
        title: 'Split segments out of "{{name}}"',
        description:
          'Tick the segments that belong to a new speaker. The new speaker gets a new role; the remaining segments stay with the original speaker.',
        note: 'Splitting only affects this session and does not keep per-segment voice embeddings — they may be grouped together again on the next diarization run.',
        loading: 'Loading segments…',
        empty: 'This speaker has no segments to split in this session.',
        emptyText: '(no text)',
        segmentAria: 'Select segment {{seq}}',
        selected: '{{count}} selected',
        cancel: 'Cancel',
        submit: 'Split',
      },

      assign: {
        title: 'Assign "{{name}}" to a role',
        description: 'Only the role mapping of this speaker in this session changes; the voice identity and segment ownership stay as they are.',
        roleLabel: 'Target role',
        placeholder: 'Pick an existing role',
        empty: 'No role to assign to yet — rename one of the speakers into a role first.',
        cancel: 'Cancel',
        submit: 'Assign',
      },
    },

    // Session playback and lyric-style sync (FR-04 UI side)
    playback: {
      close: 'Close playback',
      loading: 'Loading timeline…',
      retry: 'Retry',
      titleMeta: '{{time}} · {{source}} · {{count}} segments',
      lyricsAria: 'Transcribed segments',

      // —— Player controls ——
      play: 'Play',
      pause: 'Pause',
      back5: 'Back 5 seconds',
      forward5: 'Forward 5 seconds',
      prevSegment: 'Previous segment',
      nextSegment: 'Next segment',
      progressAria: 'Playback position, drag to seek',
      positionAria: 'Playback position {{position}} / {{duration}}',
      rateAria: 'Playback speed: {{rate}}',
      rateValue: '{{rate}}×',
      volumeAria: 'Volume',
      volumeValue: 'Volume {{percent}}%',
      mute: 'Mute',
      unmute: 'Unmute',

      // —— Segments ——
      unknownSpeaker: 'Unidentified',
      unfinalized: 'Transcribing',
      segmentRange: '{{start}} – {{end}}',
      backToActive: 'Back to current segment',

      // —— Empty / loading / error ——
      emptyTitle: 'This session has no transcript yet',
      emptyDescription:
        'Playback needs a transcript first: close this drawer, hit "Start transcription" on this row, then come back to play it segment by segment.',
      emptyTranscribing: 'Transcription is running. Segments show up here when it finishes; you can listen to the audio now.',
      emptyFailed: 'The last transcription failed. Close this drawer, hit "Retry transcription" on the row, then come back.',

      audioMissing: {
        title: 'Audio file is missing',
        message:
          'The audio for this session is no longer in the data directory (it may have been cleaned up). The transcript stays readable, but playback is unavailable.',
      },
      audioError: {
        title: 'Failed to load audio',
        message:
          'Could not read the audio stream: the file may have been moved or damaged. The transcript stays readable; export from the library if you need the text.',
        retry: 'Reload audio',
      },

      // Timeline markers (backend contract.Marker.kind)
      markers: {
        degrade: 'Engine degraded',
        'source-lost': 'Audio source lost',
        'device-switch': 'Audio device switched',
        paused: 'Recording paused',
        resumed: 'Recording resumed',
        'perf-throttle': 'Throttled by system load',
        'perf-suspend': 'Inference paused by system load',
        'perf-resume': 'Inference resumed after load dropped',
        generic: 'Recording event',
      },
    },

    // Error copy keyed by backend error code (internal/apierr); falls back to errors.unknown.
    errors: {
      unknown: 'Operation failed: {{message}}',
      dismiss: 'Dismiss message',
      ERR_INVALID_ARGUMENT: 'The request arguments were invalid. Check the filters and retry.',
      ERR_NOT_FOUND: 'The session could not be found; it may have been deleted.',
      ERR_DB_WRITE: 'Writing to the database failed. Please retry.',
      ERR_DISK_FULL: 'Not enough disk space; export or transcription cannot continue. Free some space and retry.',
      ERR_PATH_UNWRITABLE: 'The target path is not writable. Choose another location.',
      ERR_ENGINE_NOT_READY: 'The inference engine is not ready yet. Please retry shortly.',
      ERR_MODEL_NOT_FOUND: 'The current model is unavailable. Download or switch models on the Models page.',
      ERR_MODEL_CORRUPT: 'The model files are corrupt. Download the model again on the Models page.',
      ERR_LOCAL_UNAVAILABLE: 'No speaker model is installed. Download one on the Models page before starting diarization.',
      ERR_STATE_FORBIDDEN: 'This action is not allowed right now (a transcription task may already be running).',
      ERR_PERF_SUSPENDED: 'System load is too high, so inference is paused and will resume automatically.',
      ERR_OFFLINE_ENFORCED: 'Offline mode is on, so online models are unavailable.',
    },
  },
};
