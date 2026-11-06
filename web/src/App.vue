<script setup lang="ts">
import { onBeforeUnmount } from 'vue'
import { useRouter } from 'vue-router'
import { NConfigProvider, NDialogProvider, NMessageProvider, zhCN, dateZhCN } from 'naive-ui'
import { onSessionEnded } from './auth/session'

const router = useRouter()

// A session that ends on its own sends the administrator back to the login.
const stopListening = onSessionEnded(() => {
  if (router.currentRoute.value.path !== '/login') {
    void router.replace('/login')
  }
})

onBeforeUnmount(stopListening)

// The theme of the platform: the accent of the brand, the navy of the
// text and the soft corners of the cards, shared by every component of the
// library. The palette is the one of the approved design.
const themeOverrides = {
  common: {
    primaryColor: '#0b63f6',
    primaryColorHover: '#0755d8',
    primaryColorPressed: '#0648b8',
    primaryColorSuppl: '#0b63f6',
    infoColor: '#0b63f6',
    successColor: '#0bbf7a',
    warningColor: '#f79009',
    errorColor: '#f04438',
    textColorBase: '#16264a',
    textColor1: '#16264a',
    textColor2: '#34446b',
    textColor3: '#7b8aa8',
    borderColor: '#e9eff9',
    dividerColor: '#eef3fb',
    fillColor: '#f4f8ff',
    bodyColor: 'transparent',
    cardColor: '#ffffff',
    modalColor: '#ffffff',
    popoverColor: '#ffffff',
    tableColor: '#ffffff',
    tableHeaderColor: '#f8faff',
    inputColor: '#ffffff',
    borderRadius: '10px',
    borderRadiusSmall: '8px',
    fontSize: '14px',
    fontFamily:
      '"Segoe UI", system-ui, -apple-system, "PingFang SC", "Hiragino Sans GB", "Microsoft YaHei", "Noto Sans SC", sans-serif',
  },
  Card: {
    borderRadius: '16px',
    color: '#ffffff',
    borderColor: '#e9eff9',
  },
  Button: { borderRadiusMedium: '10px', borderRadiusSmall: '8px' },
  Input: { borderRadius: '10px' },
  Tag: { borderRadius: '999px' },
  Alert: { borderRadius: '12px' },
  DataTable: { borderRadius: '16px', thColor: '#f8faff' },
}
</script>

<template>
  <NConfigProvider :theme-overrides="themeOverrides" :locale="zhCN" :date-locale="dateZhCN">
    <NMessageProvider>
      <NDialogProvider>
        <RouterView />
      </NDialogProvider>
    </NMessageProvider>
  </NConfigProvider>
</template>
