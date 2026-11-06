<script setup lang="ts">
import BrandMark from './BrandMark.vue'

// The frame of the pages before the session: a full-height brand panel on
// the left and the form of the administrator on the right. The same frame
// serves the login and the initialization, so the first two pages of the
// platform read as one product.
</script>

<template>
  <div class="auth">
    <aside class="auth-visual">
      <div class="auth-glow" aria-hidden="true"></div>
      <svg class="auth-art" viewBox="0 0 800 460" preserveAspectRatio="none" aria-hidden="true">
        <defs>
          <linearGradient id="auth-wave" x1="0" y1="1" x2=".9" y2="0">
            <stop offset="0" stop-color="#2f7ef0" stop-opacity=".05" />
            <stop offset=".45" stop-color="#5ba7ff" stop-opacity=".5" />
            <stop offset="1" stop-color="#a5d8ff" stop-opacity=".75" />
          </linearGradient>
          <radialGradient id="auth-glow" cx="50%" cy="80%" r="60%">
            <stop offset="0" stop-color="#2f7ef0" stop-opacity=".5" />
            <stop offset="1" stop-color="#2f7ef0" stop-opacity="0" />
          </radialGradient>
        </defs>
        <rect x="0" y="120" width="800" height="340" fill="url(#auth-glow)" />
        <g fill="none" stroke="url(#auth-wave)" stroke-width="1.2" opacity=".6">
          <path d="M-40 440C90 300 170 380 290 290S470 120 630 160 790 100 850 130" />
          <path d="M-40 460C90 320 174 398 294 308S474 138 634 178 794 118 856 148" />
          <path d="M-40 480C90 340 178 416 298 326S478 156 638 196 798 136 862 166" />
          <path d="M-40 420C90 280 166 362 286 272S466 102 626 142 786 82 845 112" />
          <path d="M-40 400C90 260 162 344 282 254S462 84 622 124 782 64 840 94" />
          <path d="M-40 500C90 360 182 434 302 344S482 174 642 214 802 154 868 184" />
        </g>
        <g fill="none" stroke="#7fc4ff" stroke-width=".6" opacity=".16">
          <path d="M60 460C140 340 220 290 320 260M160 460C230 360 300 316 390 288M270 460C330 380 390 344 470 318M390 460C440 396 490 366 560 344M520 460C560 410 600 386 660 368" />
        </g>
      </svg>

      <div class="auth-brand">
        <BrandMark class="auth-mark" />
        <div class="auth-brand-copy">
          <strong>Docker</strong>
          <small>监控平台</small>
        </div>
      </div>

      <div class="auth-copy">
        <span class="auth-chip" aria-hidden="true">
          <BrandMark class="auth-chip-mark" />
        </span>
        <h1>Docker 集群监控平台</h1>
        <p>集中管理多站点、多服务的 Docker 运行状态，保障业务稳定运行。</p>

        <ul class="auth-points">
          <li>
            <svg viewBox="0 0 24 24" aria-hidden="true">
              <path d="M4 4h16v6H4V4Zm0 10h16v6H4v-6Z" />
            </svg>
            多站点纳管
          </li>
          <li>
            <svg viewBox="0 0 24 24" aria-hidden="true">
              <path d="M3 12h4l3-8 4 16 3-8h4" />
            </svg>
            实时监控
          </li>
          <li>
            <svg viewBox="0 0 24 24" aria-hidden="true">
              <path d="M12 3 5 5.8v5.4c0 4.2 2.9 7.6 7 9.8 4.1-2.2 7-5.6 7-9.8V5.8L12 3Z" />
              <path d="m8.8 11.6 2.2 2.2 4.2-4.4" />
            </svg>
            安全接入
          </li>
        </ul>
      </div>

      <p class="auth-foot">轻量部署 · 实时监控 · 安全接入</p>
    </aside>

    <section class="auth-panel">
      <div class="auth-form">
        <slot />
      </div>
    </section>
  </div>
</template>

<style scoped>
.auth {
  display: grid;
  grid-template-columns: minmax(0, 1.14fr) minmax(440px, 0.86fr);
  min-height: 100dvh;
  background: #fff;
}

/* The left panel ------------------------------------------------------- */
.auth-visual {
  position: relative;
  display: flex;
  flex-direction: column;
  overflow: hidden;
  padding: 44px 56px 36px;
  color: #fff;
  background:
    radial-gradient(760px 520px at 82% 8%, rgb(38 110 226 / 45%) 0%, rgb(38 110 226 / 0%) 62%),
    radial-gradient(620px 480px at 8% 96%, rgb(21 96 214 / 55%) 0%, rgb(21 96 214 / 0%) 64%),
    linear-gradient(152deg, #081a3b 0%, #0d2857 46%, #0a1f44 100%);
}

.auth-glow {
  position: absolute;
  inset: auto -20% -40% 10%;
  height: 80%;
  background: radial-gradient(closest-side, rgb(47 126 240 / 35%), rgb(47 126 240 / 0));
  filter: blur(10px);
  pointer-events: none;
}

.auth-art {
  position: absolute;
  inset: auto 0 0;
  width: 100%;
  height: 62%;
  pointer-events: none;
}

.auth-brand {
  position: relative;
  z-index: 1;
  display: flex;
  align-items: center;
  gap: 11px;
}

.auth-mark {
  width: 46px;
  height: 44px;
  filter: drop-shadow(0 4px 12px rgb(20 140 255 / 45%));
}

.auth-brand-copy {
  display: grid;
  line-height: 1.16;
}

.auth-brand-copy strong {
  font-size: 19px;
  font-weight: 700;
  letter-spacing: -0.02em;
}

.auth-brand-copy small {
  font-size: 14px;
  color: #cfdcf2;
}

.auth-copy {
  position: relative;
  z-index: 1;
  margin: auto 0;
  max-width: 33rem;
  padding: 40px 0;
}

.auth-chip {
  width: 92px;
  height: 92px;
  display: grid;
  place-items: center;
  margin-bottom: 30px;
  border: 1px solid rgb(255 255 255 / 14%);
  border-radius: 26px;
  background: rgb(255 255 255 / 7%);
  box-shadow:
    inset 0 1px 0 rgb(255 255 255 / 18%),
    0 28px 60px -24px rgb(22 110 255 / 65%);
  backdrop-filter: blur(8px);
}

.auth-chip-mark {
  width: 60px;
  height: 56px;
}

.auth-copy h1 {
  color: #fff;
  font-size: 42px;
  font-weight: 800;
  letter-spacing: -0.025em;
  line-height: 1.18;
}

.auth-copy p {
  max-width: 30rem;
  margin-top: 16px;
  color: rgb(214 228 250 / 78%);
  font-size: 15.5px;
  line-height: 1.8;
}

.auth-points {
  display: flex;
  flex-wrap: wrap;
  gap: 12px;
  margin: 34px 0 0;
  padding: 0;
  list-style: none;
}

.auth-points li {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  padding: 8px 14px;
  border: 1px solid rgb(255 255 255 / 12%);
  border-radius: 999px;
  background: rgb(255 255 255 / 6%);
  color: rgb(219 233 252 / 88%);
  font-size: 12.5px;
  font-weight: 500;
}

.auth-points svg {
  width: 16px;
  height: 16px;
  fill: none;
  stroke: #6fc1ff;
  stroke-width: 1.7;
  stroke-linecap: round;
  stroke-linejoin: round;
}

.auth-foot {
  position: relative;
  z-index: 1;
  color: rgb(190 210 238 / 55%);
  font-size: 11.5px;
  letter-spacing: 0.04em;
}

/* The panel of the form ------------------------------------------------ */
.auth-panel {
  display: grid;
  place-items: center;
  padding: 56px 48px;
  background:
    radial-gradient(560px 360px at 100% 0%, #f2f8ff 0%, rgb(242 248 255 / 0) 60%),
    #fff;
}

.auth-form {
  width: min(100%, 25rem);
}

@media (max-width: 1180px) {
  .auth {
    grid-template-columns: minmax(0, 1fr) minmax(420px, 0.9fr);
  }

  .auth-visual {
    padding: 36px 40px 30px;
  }

  .auth-copy h1 {
    font-size: 34px;
  }

  .auth-panel {
    padding: 44px 34px;
  }
}

@media (max-width: 900px) {
  .auth {
    grid-template-columns: minmax(0, 1fr);
    min-height: 100dvh;
  }

  .auth-visual {
    min-height: 246px;
    padding: 26px 26px 26px;
  }

  .auth-art {
    height: 100%;
    opacity: 0.6;
  }

  .auth-copy {
    margin: 0;
    padding: 34px 0 8px;
  }

  .auth-chip,
  .auth-points,
  .auth-foot {
    display: none;
  }

  .auth-copy h1 {
    font-size: 28px;
  }

  .auth-copy p {
    margin-top: 10px;
    font-size: 13.5px;
  }

  .auth-panel {
    display: block;
    padding: 36px 22px 48px;
  }

  .auth-form {
    width: min(100%, 27rem);
    margin: 0 auto;
  }
}
</style>
