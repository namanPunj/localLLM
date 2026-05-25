class SittingAvatar {
  constructor(containerId) {
    this.container = document.getElementById(containerId);
    
    // Injected SVG (Updated with Spectacles group)
    this.container.innerHTML = `
      <svg class="stick-figure-svg spectacles-hidden" viewBox="40 15 80 140">
        
        <g class="stickman-group">
            
          <line class="limb back-layer" x1="60" y1="45" x2="75" y2="65" />
          <line class="limb back-layer" x1="75" y1="65" x2="90" y2="75" />
          
          <line class="limb back-layer back-thigh" x1="60" y1="80" x2="95" y2="80" />
          <line class="limb back-layer back-calf" x1="95" y1="80" x2="95" y2="125" />

          <path class="torso front-layer" d="M 60 40 Q 45 65 60 85" />
          
          <g class="head-with-glasses">
              <circle class="head" cx="62" cy="24" r="16" />
              
              <g class="spectacles">
                  <circle cx="56" cy="20" r="5" class="spec-lens"/>
                  <circle cx="68" cy="20" r="5" class="spec-lens"/>
                  <path d="M 61 20 Q 62 19 63 20" class="spec-bridge"/>
                  <line x1="51" y1="20" x2="48" y2="18" class="spec-arm"/>
                  <line x1="73" y1="20" x2="76" y2="18" class="spec-arm"/>
              </g>
          </g>

          <line class="limb front-layer front-thigh" x1="60" y1="85" x2="105" y2="85" />
          <line class="limb front-layer front-calf" x1="105" y1="85" x2="105" y2="135" />
          
          <line class="limb front-layer" x1="60" y1="45" x2="80" y2="75" />
          <line class="limb front-layer" x1="80" y1="75" x2="108" y2="72" />
        </g>
      </svg>
    `;

    // Cache essential elements
    this.svg = this.container.querySelector('svg');
    this.glassesVisible = false;
  }

  // New method to toggle glasses visibility
  toggleSpectacles() {
    this.glassesVisible = !this.glassesVisible;
    if (this.glassesVisible) {
      this.svg.classList.remove('spectacles-hidden');
      this.svg.classList.add('spectacles-visible');
    } else {
      this.svg.classList.remove('spectacles-visible');
      this.svg.classList.add('spectacles-hidden');
    }
  }
}

// Function to handle the button click and toggle glasses
document.getElementById('toggle-glasses-btn').addEventListener('click', function() {
  myAvatar.toggleSpectacles();
});

// Initialize the avatar
const myAvatar = new SittingAvatar('sitting-stickman-container');