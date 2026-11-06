<script setup lang="ts">
import { useId } from 'vue'

// The mark of the platform: the whale of Docker with the containers on its
// back, drawn as a flat vector so it stays crisp in the sidebar and on the
// gate pages.
const gradientID = `whale-${useId()}`
</script>

<template>
  <svg class="brand-whale" viewBox="0 0 64 60" aria-hidden="true">
    <defs>
      <linearGradient :id="gradientID" x1="0" y1="0" x2=".7" y2="1">
        <stop offset="0" stop-color="#37c3ff" />
        <stop offset="1" stop-color="#0a8bf2" />
      </linearGradient>
    </defs>
    <g fill="#9fe2ff">
      <rect x="18" y="26" width="9" height="9" rx="2" />
      <rect x="28.5" y="26" width="9" height="9" rx="2" />
      <rect x="39" y="26" width="9" height="9" rx="2" />
      <rect x="23.2" y="16" width="9" height="9" rx="2" />
      <rect x="33.7" y="16" width="9" height="9" rx="2" />
      <rect x="28.5" y="6" width="9" height="9" rx="2" />
    </g>
    <path d="M6 36h48c0 10-8 17-18.5 17H22C11.5 53 6 46 6 36Z" :fill="`url(#${gradientID})`" />
    <path
      d="M52 36c5.6-1.4 9.4-5.4 10.4-10.6 3 3.4 2.6 8.8-1.2 12.2-2.4 2.1-5.6 3.1-9.2 3.1Z"
      :fill="`url(#${gradientID})`"
    />
    <path d="M7 42c4.6 6.2 11 9 19 9h6c-4.3-3.2-7-8-7.5-13H7Z" fill="#eaf8ff" />
    <circle cx="23" cy="39" r="2" fill="#093d86" />
  </svg>
</template>

<style scoped>
.brand-whale {
  display: block;
  flex: none;
}
</style>
