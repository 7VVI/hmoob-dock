<script setup lang="ts">
// The illustration of the overview: the whale of Docker on a stack of
// platforms, among the racks of a data center. It is drawn as a flat scene
// of soft blues, so it scales with the hero and never needs a bitmap.
</script>

<template>
  <svg class="hero-art" viewBox="0 0 1000 360" preserveAspectRatio="xMidYMax slice" aria-hidden="true">
    <defs>
      <linearGradient id="ha-whale" x1=".2" y1="0" x2=".7" y2="1">
        <stop offset="0" stop-color="#3d9bff" />
        <stop offset=".55" stop-color="#1274f2" />
        <stop offset="1" stop-color="#0a49c8" />
      </linearGradient>
      <linearGradient id="ha-whale-tail" x1="0" y1="0" x2="1" y2="1">
        <stop offset="0" stop-color="#4cb4ff" />
        <stop offset="1" stop-color="#0d55d8" />
      </linearGradient>
      <linearGradient id="ha-cube-top" x1="0" y1="0" x2="0" y2="1">
        <stop offset="0" stop-color="#83c7ff" />
        <stop offset="1" stop-color="#3f8ff5" />
      </linearGradient>
      <linearGradient id="ha-cube-front" x1="0" y1="0" x2="0" y2="1">
        <stop offset="0" stop-color="#2f83f4" />
        <stop offset="1" stop-color="#0f5fd8" />
      </linearGradient>
      <linearGradient id="ha-slab" x1="0" y1="0" x2="0" y2="1">
        <stop offset="0" stop-color="#ffffff" />
        <stop offset=".7" stop-color="#edf4ff" />
        <stop offset="1" stop-color="#dce9fd" />
      </linearGradient>
      <linearGradient id="ha-slab-front" x1="0" y1="0" x2="0" y2="1">
        <stop offset="0" stop-color="#d6e6fd" />
        <stop offset="1" stop-color="#c2d8f8" />
      </linearGradient>
      <linearGradient id="ha-rack-front" x1="0" y1="0" x2="1" y2="1">
        <stop offset="0" stop-color="#dceaff" />
        <stop offset="1" stop-color="#bdd7f8" />
      </linearGradient>
      <linearGradient id="ha-rack-side" x1="0" y1="0" x2="0" y2="1">
        <stop offset="0" stop-color="#c9dffb" />
        <stop offset="1" stop-color="#a8c8f0" />
      </linearGradient>
      <radialGradient id="ha-glow" cx="50%" cy="50%" r="50%">
        <stop offset="0" stop-color="#ffffff" stop-opacity=".95" />
        <stop offset=".6" stop-color="#eef6ff" stop-opacity=".55" />
        <stop offset="1" stop-color="#eef6ff" stop-opacity="0" />
      </radialGradient>
      <radialGradient id="ha-blueglow" cx="50%" cy="50%" r="50%">
        <stop offset="0" stop-color="#4f9dff" stop-opacity=".5" />
        <stop offset="1" stop-color="#4f9dff" stop-opacity="0" />
      </radialGradient>
    </defs>

    <g transform="translate(-84,0)">
    <!-- The light of the room -->
    <ellipse cx="650" cy="300" rx="330" ry="120" fill="url(#ha-glow)" />
    <ellipse cx="650" cy="286" rx="250" ry="60" fill="url(#ha-blueglow)" />

    <!-- The network that ties the racks to the platform -->
    <g fill="none" stroke="#5f9df0" stroke-width="1" opacity=".38">
      <path d="M244 226C330 226 356 250 470 252" />
      <path d="M760 202C708 202 690 240 640 248" />
      <path d="M792 150C846 150 872 172 892 214" />
      <path d="M258 158C330 128 390 132 452 176" />
      <path d="M880 96C930 122 944 150 940 176" />
    </g>
    <g fill="#7db8ff" opacity=".8">
      <circle cx="334" cy="240" r="2.4" />
      <circle cx="736" cy="208" r="2.4" />
      <circle cx="892" cy="214" r="2.2" />
      <circle cx="452" cy="176" r="2.2" />
      <circle cx="940" cy="176" r="2.2" />
    </g>

    <!-- The racks of the data center -->
    <g>
      <path d="M168 196 198 174H272L242 196Z" fill="#e8f2ff" />
      <path d="M168 196H242V292H168Z" fill="url(#ha-rack-front)" />
      <path d="M242 196 272 174V270L242 292Z" fill="url(#ha-rack-side)" />
      <g fill="#7fb6ef" opacity=".8">
        <rect x="180" y="210" width="46" height="4" rx="2" />
        <rect x="180" y="228" width="40" height="4" rx="2" />
        <rect x="180" y="246" width="46" height="4" rx="2" />
        <rect x="180" y="264" width="34" height="4" rx="2" />
      </g>
      <g fill="#35d39a">
        <rect x="226" y="210" width="7" height="3" rx="1.5" />
        <rect x="226" y="228" width="7" height="3" rx="1.5" />
        <rect x="226" y="246" width="7" height="3" rx="1.5" />
      </g>
    </g>

    <g>
      <path d="M782 128 818 104H896L860 128Z" fill="#e8f2ff" />
      <path d="M782 128H860V258H782Z" fill="url(#ha-rack-front)" />
      <path d="M860 128 896 104V234L860 258Z" fill="url(#ha-rack-side)" />
      <g fill="#7fb6ef" opacity=".8">
        <rect x="796" y="146" width="50" height="4" rx="2" />
        <rect x="796" y="166" width="42" height="4" rx="2" />
        <rect x="796" y="186" width="50" height="4" rx="2" />
        <rect x="796" y="206" width="38" height="4" rx="2" />
        <rect x="796" y="226" width="46" height="4" rx="2" />
      </g>
      <g fill="#35d39a">
        <rect x="844" y="146" width="7" height="3" rx="1.5" />
        <rect x="844" y="186" width="7" height="3" rx="1.5" />
        <rect x="844" y="226" width="7" height="3" rx="1.5" />
      </g>
    </g>

    <g opacity=".9">
      <path d="M676 70 694 58H728L710 70Z" fill="#edf5ff" />
      <path d="M676 70H710V116H676Z" fill="url(#ha-rack-front)" />
      <path d="M710 70 728 58V104L710 116Z" fill="url(#ha-rack-side)" />
      <g fill="#8cc0f2">
        <rect x="683" y="80" width="20" height="3" rx="1.5" />
        <rect x="683" y="90" width="16" height="3" rx="1.5" />
        <rect x="683" y="100" width="20" height="3" rx="1.5" />
      </g>
      <rect x="703" y="80" width="5" height="2.5" rx="1.2" fill="#35d39a" />
    </g>

    <!-- The floating cubes -->
    <g opacity=".85">
      <path d="M312 128 332 116H358L338 128Z" fill="#9cccff" />
      <path d="M312 128H338V154H312Z" fill="#6fb1fa" />
      <path d="M338 128 358 116V142L338 154Z" fill="#4b95ee" />
    </g>
    <g opacity=".55">
      <path d="M920 210 934 201H954L940 210Z" fill="#a9d4ff" />
      <path d="M920 210H940V232H920Z" fill="#83bcf7" />
      <path d="M940 210 954 201V223L940 232Z" fill="#67a7ef" />
    </g>
    <g opacity=".5">
      <path d="M104 236 120 226H144L128 236Z" fill="#b4d9ff" />
      <path d="M104 236H128V258H104Z" fill="#93c5f8" />
      <path d="M128 236 144 226V248L128 258Z" fill="#77b0ef" />
    </g>

    <!-- The platform that holds the whale -->
    <g transform="translate(-90,0)">
      <ellipse cx="648" cy="316" rx="250" ry="26" fill="#bcd6f8" opacity=".45" />
      <rect x="470" y="300" width="440" height="36" rx="18" fill="url(#ha-slab)" />
      <rect x="470" y="322" width="440" height="14" rx="7" fill="url(#ha-slab-front)" opacity=".9" />
      <rect x="498" y="268" width="384" height="34" rx="17" fill="url(#ha-slab)" />
      <rect x="498" y="290" width="384" height="12" rx="6" fill="url(#ha-slab-front)" opacity=".9" />
      <rect x="522" y="292" width="300" height="4" rx="2" fill="#2f8cff" opacity=".55" />
      <rect x="530" y="232" width="322" height="36" rx="18" fill="url(#ha-slab)" />
      <rect x="530" y="256" width="322" height="13" rx="6.5" fill="url(#ha-slab-front)" opacity=".95" />
      <rect x="556" y="258" width="252" height="5" rx="2.5" fill="#2f8cff" opacity=".85" />
    </g>

    <!-- The whale -->
    <g transform="translate(-90,0)">
      <ellipse cx="638" cy="250" rx="110" ry="10" fill="#0f4fbf" opacity=".22" />
      <path d="M526 176C528 214 574 250 638 250C704 250 750 214 754 176Z" fill="url(#ha-whale)" />
      <path
        d="M750 178C772 158 788 128 780 100C810 116 814 154 792 178C778 192 760 196 746 194Z"
        fill="url(#ha-whale-tail)"
      />
      <path
        d="M744 180C760 168 770 150 768 132C782 142 786 162 776 178C770 186 760 192 748 194Z"
        fill="#0a49c8"
        opacity=".35"
      />
      <path
        d="M528 196C536 218 552 238 574 250C596 260 626 262 654 256C626 252 600 240 582 222C568 208 554 198 542 192C536 193 532 195 528 196Z"
        fill="#eaf8ff"
        opacity=".95"
      />
      <path
        d="M534 214C552 238 580 250 612 252"
        fill="none"
        stroke="#0a49c8"
        stroke-width="2"
        stroke-linecap="round"
        opacity=".25"
      />
      <ellipse cx="588" cy="192" rx="24" ry="11" fill="#ffffff" opacity=".16" transform="rotate(-20 588 192)" />
      <circle cx="580" cy="198" r="5" fill="#f2faff" />
      <circle cx="581" cy="199" r="2.1" fill="#093d86" opacity=".55" />
      <g fill="#ffffff" opacity=".75">
        <circle cx="612" cy="206" r="1.5" />
        <circle cx="620" cy="201" r="1.1" />
      </g>

      <!-- The containers on the back of the whale -->
      <g>
        <path d="M564 158 572 150H602L594 158Z" fill="url(#ha-cube-top)" />
        <rect x="564" y="158" width="30" height="20" rx="2" fill="url(#ha-cube-front)" />
        <path d="M594 158 602 150V170L594 178Z" fill="#0a4fbe" />
        <path d="M600 158 608 150H638L630 158Z" fill="url(#ha-cube-top)" />
        <rect x="600" y="158" width="30" height="20" rx="2" fill="url(#ha-cube-front)" />
        <path d="M630 158 638 150V170L630 178Z" fill="#0a4fbe" />
        <path d="M636 158 644 150H674L666 158Z" fill="url(#ha-cube-top)" />
        <rect x="636" y="158" width="30" height="20" rx="2" fill="url(#ha-cube-front)" />
        <path d="M666 158 674 150V170L666 178Z" fill="#0a4fbe" />

        <path d="M582 136 590 128H620L612 136Z" fill="url(#ha-cube-top)" />
        <rect x="582" y="136" width="30" height="20" rx="2" fill="url(#ha-cube-front)" />
        <path d="M612 136 620 128V148L612 156Z" fill="#0a4fbe" />
        <path d="M618 136 626 128H656L648 136Z" fill="url(#ha-cube-top)" />
        <rect x="618" y="136" width="30" height="20" rx="2" fill="url(#ha-cube-front)" />
        <path d="M648 136 656 128V148L648 156Z" fill="#0a4fbe" />

        <path d="M600 114 608 106H638L630 114Z" fill="url(#ha-cube-top)" />
        <rect x="600" y="114" width="30" height="20" rx="2" fill="url(#ha-cube-front)" />
        <path d="M630 114 638 106V126L630 134Z" fill="#0a4fbe" />
      </g>
    </g>
    </g>
  </svg>
</template>

<style scoped>
.hero-art {
  display: block;
  width: 100%;
  height: 100%;
}
</style>
